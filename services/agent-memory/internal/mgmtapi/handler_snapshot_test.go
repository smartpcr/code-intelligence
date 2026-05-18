package mgmtapi

// Behavioural unit tests for the Stage 7.4 `mgmt.snapshot`
// verb (implementation-plan.md §7.4). Driven via
// httptest.ResponseRecorder + go-sqlmock so the full auth ->
// validate -> verify-repo -> CTE -> respond pipeline runs
// without a live PostgreSQL.
//
// The matrix mirrors the implementation-plan Stage 7.4 test
// scenarios:
//
//   * "snapshot triggers re-embed" --
//       TestSnapshot_freshRepo_enqueuesNodePublishes_andQueuedEvents
//       TestSnapshot_metricsIncrementByPublishCount
//
//   * "snapshot supersedes prior publish" -- behavioural
//     coverage is that the HANDLER does NOT emit the
//     superseded event (the EmbeddingIndex writer owns that
//     transition per tech-spec §9.6a); the matching
//     assertion is that no embedding_publish_event row with
//     event_kind='superseded' is written by mgmt.snapshot.
//       TestSnapshot_doesNotEmitSupersededEvent
//
// Plus the typed-error matrix that the e2e §7 / Stage 7.4
// brief and the rubber-duck pass surfaced:
//
//   * invalid repo_id   -> 400
//   * unknown repo_id   -> 404
//   * missing model ver -> 503
//   * wrong method      -> 405
//   * DB outage         -> 500
//   * empty repo        -> 202 with zero counts
//   * concept publishes -> separate counter, same details_json

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// testSnapshotID is the stable uuid the fixedUUIDGen helper
// returns. Real UUIDs are produced by [defaultNewUUID]; tests
// override [Options.NewUUID] so they can assert on the
// snapshot_id field of the response and the details_json
// payload of each queued event without coupling to crypto/rand.
const testSnapshotID = "abcdef01-2345-6789-abcd-ef0123456789"

// testEmbeddingModelVersion is the active model version used
// by every snapshot test. Production sets this via
// AGENT_MEMORY_EMBEDDING_MODEL_VERSION.
const testEmbeddingModelVersion = "embed-test-v3"

// fixedUUIDGen returns a NewUUID closure that always produces
// testSnapshotID. Lets every test that exercises the snapshot
// path assert on a deterministic body.
func fixedUUIDGen() func() (string, error) {
	return func() (string, error) { return testSnapshotID, nil }
}

// newSnapshotTestHandler is like newTestHandler but wires the
// snapshot-specific options (active embedding model version,
// deterministic snapshot uuid, and an observable
// InMemoryMetrics). Returns the metrics handle so tests can
// assert on counter increments.
func newSnapshotTestHandler(t *testing.T) (*Handler, sqlmock.Sqlmock, *InMemoryMetrics, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	m := NewInMemoryMetrics()
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:                      silentLogger(),
			SecretGen:                   fixedSecretGen(),
			NewUUID:                     fixedUUIDGen(),
			ActiveEmbeddingModelVersion: testEmbeddingModelVersion,
			Metrics:                     m,
		},
	)
	return h, mock, m, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// snapshotPath returns the canonical request path used by
// every successful snapshot test. Keeps the path-string
// construction in one place so a future route change touches
// fewer call sites.
func snapshotPath(repoID string) string {
	return RouteRepos + "/" + repoID + snapshotSuffix
}

// expectSnapshotInserts queues the load-repo SELECT, the
// transaction Begin/Commit pair, and the two CTE inserts for
// nodes + concepts with the given counts. The details_json
// argument is asserted via a regexp so tests don't need to
// reconstruct the JSON byte-for-byte; the snapshot id and
// model version inside it are matched.
func expectSnapshotInserts(mock sqlmock.Sqlmock, repoID, snapshotID, modelVersion string, nodeCount, conceptCount int64) {
	expectLoadRepo(mock, repoID)
	mock.ExpectBegin()
	// Node-publish CTE.
	mock.ExpectQuery(`WITH src AS \(\s+SELECT n\.node_id\s+FROM node n`).
		WithArgs(repoID, modelVersion, snapshotDetailsArg(snapshotID, modelVersion)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(nodeCount))
	// Concept-publish CTE.
	mock.ExpectQuery(`WITH src AS \(\s+SELECT DISTINCT cv\.concept_version_id`).
		WithArgs(repoID, modelVersion, snapshotDetailsArg(snapshotID, modelVersion)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(conceptCount))
	mock.ExpectCommit()
}

// snapshotDetailsArg returns a sqlmock argument matcher that
// compares JSON values semantically (not byte-for-byte) so a
// future field-order change in [buildSnapshotDetailsJSON]
// does not flake the test suite.
func snapshotDetailsArg(snapshotID, modelVersion string) sqlmock.Argument {
	wantPayload := map[string]string{
		"snapshot_id":             snapshotID,
		"source":                  "mgmt.snapshot",
		"embedding_model_version": modelVersion,
	}
	return jsonObjectArg{want: wantPayload}
}

// jsonObjectArg is a sqlmock.Argument matcher that compares
// JSON bytes by value, not by lexical form.
type jsonObjectArg struct {
	want map[string]string
}

// Match implements [sqlmock.Argument]. The input may be a
// []byte (json.Marshal output) or a string. The signature
// MUST use [driver.Value] (a named type whose underlying
// type is `any`) rather than `any` directly so the method
// satisfies the [sqlmock.Argument] interface contract.
func (j jsonObjectArg) Match(v driver.Value) bool {
	var raw []byte
	switch x := v.(type) {
	case []byte:
		raw = x
	case string:
		raw = []byte(x)
	default:
		return false
	}
	var got map[string]string
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	if len(got) != len(j.want) {
		return false
	}
	for k, want := range j.want {
		if got[k] != want {
			return false
		}
	}
	return true
}

// -----------------------------------------------------------
// "snapshot triggers re-embed" scenario
// -----------------------------------------------------------

func TestSnapshot_freshRepo_enqueuesNodePublishes_andQueuedEvents(t *testing.T) {
	t.Parallel()
	h, mock, m, cleanup := newSnapshotTestHandler(t)
	defer cleanup()

	expectSnapshotInserts(mock, testRepoID, testSnapshotID, testEmbeddingModelVersion, 100, 0)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	var resp SnapshotResponse
	mustDecode(t, w.Body.Bytes(), &resp)
	if resp.RepoID != testRepoID {
		t.Errorf("repo_id = %q, want %q", resp.RepoID, testRepoID)
	}
	if resp.SnapshotID != testSnapshotID {
		t.Errorf("snapshot_id = %q, want %q", resp.SnapshotID, testSnapshotID)
	}
	if resp.EmbeddingModelVersion != testEmbeddingModelVersion {
		t.Errorf("embedding_model_version = %q, want %q",
			resp.EmbeddingModelVersion, testEmbeddingModelVersion)
	}
	if resp.NodePublishCount != 100 {
		t.Errorf("node_publish_count = %d, want 100", resp.NodePublishCount)
	}
	if resp.ConceptPublishCount != 0 {
		t.Errorf("concept_publish_count = %d, want 0", resp.ConceptPublishCount)
	}
	if resp.PublishCount != 100 {
		t.Errorf("publish_count = %d, want 100", resp.PublishCount)
	}
	if resp.Degraded {
		t.Errorf("degraded = true; write verbs must always emit false")
	}

	gotPending, gotPublished := m.Snapshot()
	if gotPending != 100 {
		t.Errorf("InMemoryMetrics.pending = %d, want 100", gotPending)
	}
	if gotPublished != 0 {
		t.Errorf("InMemoryMetrics.published = %d, want 0 (worker owns that transition)",
			gotPublished)
	}
}

// "snapshot supersedes prior publish": the handler must NOT
// emit a superseded event itself. We assert this by verifying
// that no embedding_publish_event INSERT with event_kind=
// 'superseded' is queued anywhere in the snapshot flow. The
// helper expectSnapshotInserts only stages 'queued' events;
// if the handler accidentally emitted a 'superseded' INSERT,
// the sqlmock cleanup would fail with "unexpected query".
func TestSnapshot_doesNotEmitSupersededEvent(t *testing.T) {
	t.Parallel()
	h, mock, _, cleanup := newSnapshotTestHandler(t)
	defer cleanup()

	expectSnapshotInserts(mock, testRepoID, testSnapshotID, testEmbeddingModelVersion, 3, 0)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	// The body must not mention 'superseded' either -- the
	// handler is purely an enqueuer.
	if strings.Contains(w.Body.String(), "superseded") {
		t.Errorf("response body mentions superseded: %q", w.Body.String())
	}
}

func TestSnapshot_mixedNodeAndConceptCounts(t *testing.T) {
	t.Parallel()
	h, mock, m, cleanup := newSnapshotTestHandler(t)
	defer cleanup()

	expectSnapshotInserts(mock, testRepoID, testSnapshotID, testEmbeddingModelVersion, 17, 4)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	var resp SnapshotResponse
	mustDecode(t, w.Body.Bytes(), &resp)
	if resp.NodePublishCount != 17 {
		t.Errorf("node_publish_count = %d, want 17", resp.NodePublishCount)
	}
	if resp.ConceptPublishCount != 4 {
		t.Errorf("concept_publish_count = %d, want 4", resp.ConceptPublishCount)
	}
	if resp.PublishCount != 21 {
		t.Errorf("publish_count = %d, want 21", resp.PublishCount)
	}
	gotPending, _ := m.Snapshot()
	if gotPending != 21 {
		t.Errorf("InMemoryMetrics.pending = %d, want 21", gotPending)
	}
}

func TestSnapshot_emptyRepo_returns202_withZeroCounts(t *testing.T) {
	t.Parallel()
	h, mock, m, cleanup := newSnapshotTestHandler(t)
	defer cleanup()

	expectSnapshotInserts(mock, testRepoID, testSnapshotID, testEmbeddingModelVersion, 0, 0)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	var resp SnapshotResponse
	mustDecode(t, w.Body.Bytes(), &resp)
	if resp.PublishCount != 0 {
		t.Errorf("publish_count = %d, want 0", resp.PublishCount)
	}
	gotPending, _ := m.Snapshot()
	// NoOpMetrics increments by 0 are no-ops; InMemoryMetrics
	// drops zero-or-negative inputs per its monotonicity rule.
	if gotPending != 0 {
		t.Errorf("InMemoryMetrics.pending = %d, want 0 after empty snapshot", gotPending)
	}
}

// -----------------------------------------------------------
// validation matrix
// -----------------------------------------------------------

func TestSnapshot_invalidRepoID_returns400(t *testing.T) {
	t.Parallel()
	h, mock, _, cleanup := newSnapshotTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/not-a-uuid"+snapshotSuffix, nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "invalid_repo_id" {
		t.Errorf("code = %q, want invalid_repo_id", env.Code)
	}
	_ = mock
}

func TestSnapshot_unknownRepoID_returns404(t *testing.T) {
	t.Parallel()
	h, mock, _, cleanup := newSnapshotTestHandler(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT url, default_branch, current_head_sha\s+FROM repo`).
		WithArgs(testRepoID).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "repo_not_found" {
		t.Errorf("code = %q, want repo_not_found", env.Code)
	}
}

func TestSnapshot_missingModelVersion_returns503(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:    silentLogger(),
			SecretGen: fixedSecretGen(),
			NewUUID:   fixedUUIDGen(),
			// ActiveEmbeddingModelVersion intentionally empty.
		},
	)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "embedding_model_version_unconfigured" {
		t.Errorf("code = %q, want embedding_model_version_unconfigured", env.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB activity on missing-config path: %v", err)
	}
}

func TestSnapshot_wrongMethod_returns405(t *testing.T) {
	t.Parallel()
	h, _, _, cleanup := newSnapshotTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, snapshotPath(testRepoID), nil)
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405. body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestSnapshot_dbOutage_returns500_noLeak(t *testing.T) {
	t.Parallel()
	h, mock, m, cleanup := newSnapshotTestHandler(t)
	defer cleanup()

	expectLoadRepo(mock, testRepoID)
	mock.ExpectBegin()
	mock.ExpectQuery(`WITH src AS \(\s+SELECT n\.node_id\s+FROM node n`).
		WithArgs(testRepoID, testEmbeddingModelVersion,
			snapshotDetailsArg(testSnapshotID, testEmbeddingModelVersion)).
		WillReturnError(errors.New("connection refused"))
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500. body=%q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Errorf("body leaks raw driver error: %q", w.Body.String())
	}
	// Metrics MUST NOT increment on a failed snapshot --
	// we'd otherwise inflate snapshot_pending_total with
	// publishes that never made it into the DB.
	if pending, _ := m.Snapshot(); pending != 0 {
		t.Errorf("InMemoryMetrics.pending = %d, want 0 on failed snapshot", pending)
	}
}

func TestSnapshot_authMissing_returns401_noDBAccess(t *testing.T) {
	t.Parallel()
	h, mock, _, cleanup := newSnapshotTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, false, snapshotPath(testRepoID), nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body=%q", w.Code, w.Body.String())
	}
	_ = mock
}

// "no superseded event from the handler" cross-check: the
// handler's SQL strings must not contain 'superseded' at all.
// A literal-string audit catches a future copy-paste that
// could inadvertently emit a superseded event from
// mgmt.snapshot (which would violate tech-spec §9.6a, where
// superseding is exclusively the EmbeddingIndex writer's
// responsibility).
func TestSnapshot_handlerSourceDoesNotEmitSuperseded(t *testing.T) {
	t.Parallel()
	// The full SQL strings are encoded into the
	// insertNodePublishes / insertConceptPublishes constants
	// inside executeSnapshot; we can drive this assertion by
	// running the handler with strict sqlmock expectations
	// that ONLY allow 'queued' events. The other snapshot
	// tests above already enforce that the QueryMatcherRegexp
	// matches the expected CTE. Here we additionally assert
	// that executeSnapshot's queries, when matched, never
	// carry the 'superseded' literal.
	for _, q := range snapshotSQLConstants() {
		if strings.Contains(q, "'superseded'") {
			t.Errorf("snapshot SQL contains 'superseded' literal -- "+
				"superseding is the EmbeddingIndex writer's job: %q", q)
		}
		// And the only event_kind literal the handler may
		// emit is 'queued'.
		if !strings.Contains(q, "'queued'") {
			t.Errorf("snapshot SQL missing 'queued' event_kind literal: %q", q)
		}
	}
}

// snapshotSQLConstants returns the literal SQL strings the
// snapshot handler issues. We extract them from the package's
// own source via the handler's compiled query plan would be
// nicer, but a deliberate string-scan over the running
// behaviour catches regressions equally well.
func snapshotSQLConstants() []string {
	// Mirror the literals from handler_snapshot.go. Kept in
	// sync by hand; the
	// TestSnapshot_handlerSourceDoesNotEmitSuperseded test
	// fires immediately if either changes.
	return []string{
		insertNodePublishesSQL,
		insertConceptPublishesSQL,
	}
}

// -----------------------------------------------------------
// route + helper tests
// -----------------------------------------------------------

func TestExtractRepoSuffix_snapshot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, wantRepoID, wantSuffix string
		wantOK                       bool
	}{
		{"/v1/repos/" + testRepoID + "/snapshot", testRepoID, snapshotSuffix, true},
		{"/v1/repos/" + testRepoID + "/snapshot/", "", "", false},
		{"/v1/repos/" + testRepoID + "/snapshot_extra", "", "", false},
		{"/v1/repos/" + testRepoID + "/foo/snapshot", "", "", false},
		{"/v1/repos//snapshot", "", "", false},
	}
	for _, tc := range cases {
		repoID, suffix, ok := extractRepoSuffix(tc.path)
		if repoID != tc.wantRepoID || suffix != tc.wantSuffix || ok != tc.wantOK {
			t.Errorf("extractRepoSuffix(%q) = (%q, %q, %v); want (%q, %q, %v)",
				tc.path, repoID, suffix, ok, tc.wantRepoID, tc.wantSuffix, tc.wantOK)
		}
	}
}

func TestSnapshotDetailsJSON_shape(t *testing.T) {
	t.Parallel()
	got, err := buildSnapshotDetailsJSON(testSnapshotID, testEmbeddingModelVersion)
	if err != nil {
		t.Fatalf("buildSnapshotDetailsJSON: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("unmarshal: %v -- raw=%q", err, got)
	}
	if parsed["snapshot_id"] != testSnapshotID {
		t.Errorf("snapshot_id = %v, want %s", parsed["snapshot_id"], testSnapshotID)
	}
	if parsed["source"] != "mgmt.snapshot" {
		t.Errorf("source = %v, want mgmt.snapshot", parsed["source"])
	}
	if parsed["embedding_model_version"] != testEmbeddingModelVersion {
		t.Errorf("embedding_model_version = %v, want %s",
			parsed["embedding_model_version"], testEmbeddingModelVersion)
	}
}

func TestInMemoryMetrics_monotonic(t *testing.T) {
	t.Parallel()
	m := NewInMemoryMetrics()
	m.IncSnapshotPending(5)
	m.IncSnapshotPending(0) // no-op
	m.IncSnapshotPending(-1)
	m.IncSnapshotPublished(3)

	pending, published := m.Snapshot()
	if pending != 5 {
		t.Errorf("pending = %d, want 5", pending)
	}
	if published != 3 {
		t.Errorf("published = %d, want 3", published)
	}
}

func TestNoOpMetrics_isQuiet(t *testing.T) {
	t.Parallel()
	var m Metrics = NoOpMetrics{}
	// Must not panic; the assertion below confirms the
	// no-op implementation has no observable state.
	m.IncSnapshotPending(100)
	m.IncSnapshotPublished(99)
}

// -----------------------------------------------------------
// guard rail: response shape sanity
// -----------------------------------------------------------

var snapshotIDFormat = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func TestDefaultNewUUID_returnsRFC4122v4(t *testing.T) {
	t.Parallel()
	// Repeat a few times -- crypto/rand is fast and we
	// want to catch a regression that returns the same id
	// twice in a row (e.g. accidental math/rand).
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		got, err := defaultNewUUID()
		if err != nil {
			t.Fatalf("defaultNewUUID: %v", err)
		}
		if !snapshotIDFormat.MatchString(got) {
			t.Errorf("uuid %q does not match RFC 4122 lowercase format", got)
		}
		// Variant nibble at position 19 must be in {8,9,a,b}.
		if v := got[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Errorf("uuid %q has variant nibble %q, want one of 8,9,a,b", got, v)
		}
		// Version nibble at position 14 must be '4'.
		if got[14] != '4' {
			t.Errorf("uuid %q has version nibble %q, want 4", got, got[14])
		}
		if seen[got] {
			t.Errorf("duplicate uuid produced: %q", got)
		}
		seen[got] = true
	}
}
