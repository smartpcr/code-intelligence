package mgmtapi

// Stage 8.1 / e2e §13 contract test for the mgmt.read.*
// verbs. Mirrors observe_faultinjection_test.go for the
// management side. Every `mgmt.read.*` response runs through
// `writeReadResponse`, which:
//
//   1. Consults `h.degradedHealthSource` for a closed-set
//      reason.  A probe error is treated as healthy.
//   2. Overlays a higher-priority injected reason on top
//      via `h.degradedFaultInjector`.
//   3. Runs `degraded.Enforce` on the final pair; a
//      non-closed reason wraps `ErrUnknownReason` and the
//      handler returns 500.
//   4. Bumps the per-verb counter when the surviving
//      reason is a closed-set value.
//
// Two scenarios are pinned here to bound the test surface
// to the same depth as the agent-side fault-injection
// tests (closed-set rewrite + non-closed 500). Coverage
// for the per-verb constant set ranges across multiple
// verbs (`mgmt.read.repos` for the closed-set test,
// `mgmt.read.episodes` for the open-set 500) so the
// chokepoint is exercised across more than one routing
// path.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/degraded"
)

// newFaultInjectionReadHandler wires a Handler against
// sqlmock plus the Stage 8.1 contract test seams (health
// source + fault injector + degraded counter). The health
// source is always nil — the §13 scenario exercises the
// injector path; an integration-style "real outage" test is
// out of scope for the unit suite (the production
// composition root wires PG-backed `MgmtHealthSource`).
func newFaultInjectionReadHandler(
	t *testing.T,
	frozen time.Time,
	fi degraded.FaultInjector,
	metric degraded.Metric,
) (*Handler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:                slog.New(slog.NewTextHandler(io.Discard, nil)),
			SecretGen:             fixedSecretGen(),
			Clock:                 func() time.Time { return frozen },
			DegradedFaultInjector: fi,
			DegradedMetric:        metric,
		},
	)
	return h, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// TestMgmtRead_faultInjection_closedSetReason_overlay pins
// the §13 happy-path: with a CLOSED-SET reason pinned on
// `mgmt.read.repos`, the response is rewritten with
// `degraded=true` + the injected reason AND the per-verb
// counter increments.
func TestMgmtRead_faultInjection_closedSetReason_overlay(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbReadRepos, degraded.ReasonEmbeddingIndexUnavailable)

	h, mock, cleanup := newFaultInjectionReadHandler(t, frozen, fi, metric)
	defer cleanup()

	mock.ExpectQuery(`SELECT r\.repo_id::text.*FROM repo r.*LEFT JOIN LATERAL.*FROM ingest_jobs`).
		WithArgs("", readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "url", "default_branch", "current_head_sha", "created_at",
			"latest_job_id", "latest_status", "latest_mode", "latest_updated_at",
		}).AddRow(
			testRepoID, testRepoURL, testBranch, testHeadSHA, frozen.Add(-time.Hour),
			"22222222-2222-2222-2222-222222222222", "done", "full", frozen.Add(-30*time.Minute),
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/repos"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with degraded envelope, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if v, _ := body["degraded"].(bool); !v {
		t.Fatalf("body.degraded=false, want true under injection (body=%#v)", body)
	}
	if got := body["degraded_reason"]; got != degraded.ReasonEmbeddingIndexUnavailable {
		t.Fatalf("body.degraded_reason=%v, want %q",
			got, degraded.ReasonEmbeddingIndexUnavailable)
	}
	if got := metric.Count(VerbReadRepos, degraded.ReasonEmbeddingIndexUnavailable); got != 1 {
		t.Fatalf("metric increment under injection = %d, want 1", got)
	}
}

// TestMgmtRead_faultInjection_nonClosedReason_returns500
// pins the §13 invariant: a non-closed reason injected on
// any mgmt.read verb MUST surface as a clean 500 with an
// `internal_error` envelope (the closed-set guard in
// `writeReadResponse` short-circuits before the success
// path).
func TestMgmtRead_faultInjection_nonClosedReason_returns500(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbReadEpisodes, "oops")

	h, mock, cleanup := newFaultInjectionReadHandler(t, frozen, fi, metric)
	defer cleanup()

	since := frozen.Add(-24 * time.Hour)
	mock.ExpectQuery(`FROM episode e`).
		WithArgs(sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"episode_id", "episode_group_id", "repo_id",
			"session_id", "trace_id", "kind", "outcome",
			"parent_episode_id", "context_id",
			"degraded", "degraded_reason",
			"created_at",
			"current_status", "status_updated_at",
		}))

	rec := httptest.NewRecorder()
	url := "/v1/episodes?since=" + since.UTC().Format(time.RFC3339)
	h.ServeHTTP(rec, readReq(t, true, url))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on non-closed injection, got %d: %s", rec.Code, rec.Body.String())
	}
	var env ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error: %v body=%q", err, rec.Body.String())
	}
	if env.Code != "internal_error" {
		t.Fatalf("env.Code=%q, want internal_error", env.Code)
	}
	// The non-closed reason MUST NOT increment the per-verb
	// counter — the counter is a closed-set instrument.
	if got := metric.Count(VerbReadEpisodes, "oops"); got != 0 {
		t.Fatalf("metric MUST NOT count non-closed reasons, got %d", got)
	}
}

// TestMgmtRead_healthProbeError_treatedAsHealthy pins the
// §8.3-style invariant: a flaky `repo_health` probe MUST NOT
// fail the read. The handler logs a Warn and serves a
// `degraded=false` envelope.
func TestMgmtRead_healthProbeError_treatedAsHealthy(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	// Health source that always errors.
	hs := MgmtHealthSourceFunc(func(ctx context.Context, verb, repoID string) (string, error) {
		return "", errFakeProbe
	})

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
			SecretGen:            fixedSecretGen(),
			Clock:                func() time.Time { return frozen },
			DegradedHealthSource: hs,
			DegradedMetric:       metric,
		},
	)

	mock.ExpectQuery(`SELECT r\.repo_id::text.*FROM repo r.*LEFT JOIN LATERAL.*FROM ingest_jobs`).
		WithArgs("", readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "url", "default_branch", "current_head_sha", "created_at",
			"latest_job_id", "latest_status", "latest_mode", "latest_updated_at",
		}).AddRow(
			testRepoID, testRepoURL, testBranch, testHeadSHA, frozen.Add(-time.Hour),
			"22222222-2222-2222-2222-222222222222", "done", "full", frozen.Add(-30*time.Minute),
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/repos"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 despite probe error, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if v, _ := body["degraded"].(bool); v {
		t.Fatalf("body.degraded=true, want false (probe error → healthy)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// errFakeProbe is the canned error returned by the test
// health source above. A package-level value keeps the
// `errors.Is` comparison cheap if a follow-up test wants
// it.
var errFakeProbe = &probeFailure{msg: "fake probe down"}

type probeFailure struct{ msg string }

func (p *probeFailure) Error() string { return p.msg }

// ---------------------------------------------------------------
// Per-verb closed-set fault-injection coverage (iter-3 / evaluator
// finding #5). Each subtest pins a closed-set reason on the
// `MgmtHealthSource` for one mgmt.read verb, mocks the
// underlying SQL the verb runs, and asserts:
//
//   1. The HTTP response is 200 with `degraded=true` and the
//      pinned closed-set reason on the wire envelope.
//   2. The per-verb counter increments under (verb, reason).
//   3. The pinned reason is the same one the operator-side
//      dashboard scrapes via /metrics.
//
// Subtests are kept individual (not parametrised) because each
// verb has a unique URL + SQL mock shape that does not factor
// cleanly into a table. The closed-set reason chosen for each
// verb is the one that operators most plausibly see for that
// verb's underlying subsystem (graph reads → graph_store, etc.)
// so the test doubles as documentation of the verb/reason
// pairing operators should expect on the dashboard.

// healthSrcReason wires a `MgmtHealthSource` that returns the
// supplied closed-set reason for every probe (any verb / any
// repoID). The §13 chokepoint runs Enforce on the resulting
// `(degraded=true, reason)` envelope before metrics.
func healthSrcReason(reason string) MgmtHealthSourceFunc {
	return func(ctx context.Context, verb, repoID string) (string, error) {
		return reason, nil
	}
}

// newPerVerbHealthHandler is the iter-3 equivalent of
// newFaultInjectionReadHandler that wires the HealthSource arm
// of the §13 contract (rather than the fault injector arm).
// Both arms funnel through the same `writeReadResponse`
// chokepoint, but a positive-health probe path proves the
// production-style wiring works end-to-end on every verb (the
// fault injector is a test-only seam).
func newPerVerbHealthHandler(
	t *testing.T,
	frozen time.Time,
	hs MgmtHealthSource,
	metric degraded.Metric,
) (*Handler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
			SecretGen:            fixedSecretGen(),
			Clock:                func() time.Time { return frozen },
			DegradedHealthSource: hs,
			DegradedMetric:       metric,
		},
	)
	return h, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// assertDegradedEnvelope decodes a JSON read response and pins
// the `(degraded=true, reason)` pair on the wire envelope.
// Routed through a helper so each subtest stays under 20 lines.
func assertDegradedEnvelope(t *testing.T, rec *httptest.ResponseRecorder, wantReason string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 with degraded envelope, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if v, _ := body["degraded"].(bool); !v {
		t.Fatalf("body.degraded=false, want true (probe pinned %q); body=%#v", wantReason, body)
	}
	if got := body["degraded_reason"]; got != wantReason {
		t.Fatalf("body.degraded_reason=%v, want %q", got, wantReason)
	}
}

func TestMgmtReadCommits_healthDegraded_emitsClosedSetEnvelope(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	reason := degraded.ReasonSpanIngestorBackpressure
	h, mock, cleanup := newPerVerbHealthHandler(t, frozen, healthSrcReason(reason), metric)
	defer cleanup()

	mock.ExpectQuery(`SELECT sha.*FROM repo_commit`).
		WithArgs(testRepoID, sqlmock.AnyArg(), false, readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{"sha", "parent_sha", "committed_at", "index_status"}).
			AddRow(testHeadSHA, "", frozen.Add(-time.Hour), "indexed"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/commits?repo_id="+testRepoID))
	assertDegradedEnvelope(t, rec, reason)
	if got := metric.Count(VerbReadCommits, reason); got != 1 {
		t.Fatalf("metric.Count(%q, %q) = %d, want 1", VerbReadCommits, reason, got)
	}
}

func TestMgmtReadObservations_healthDegraded_emitsClosedSetEnvelope(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	reason := degraded.ReasonEpisodicLogUnavailable
	h, mock, cleanup := newPerVerbHealthHandler(t, frozen, healthSrcReason(reason), metric)
	defer cleanup()

	episodeID := testRepoID
	parentCreatedAt := frozen.Add(-time.Hour)
	mock.ExpectQuery(`SELECT created_at FROM episode WHERE episode_id`).
		WithArgs(episodeID).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(parentCreatedAt))
	mock.ExpectQuery(`FROM observation\s+WHERE episode_id = .* AND created_at >=`).
		WithArgs(episodeID, parentCreatedAt).
		WillReturnRows(sqlmock.NewRows([]string{
			"observation_id", "role", "node_id", "edge_id", "concept_id",
			"degraded_recall_context_id", "weight", "created_at",
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/observations?episode_id="+episodeID))
	assertDegradedEnvelope(t, rec, reason)
	if got := metric.Count(VerbReadObservations, reason); got != 1 {
		t.Fatalf("metric.Count(%q, %q) = %d, want 1", VerbReadObservations, reason, got)
	}
}

func TestMgmtReadContext_healthDegraded_emitsClosedSetEnvelope(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	reason := degraded.ReasonGraphStoreUnavailable
	h, mock, cleanup := newPerVerbHealthHandler(t, frozen, healthSrcReason(reason), metric)
	defer cleanup()

	ctxID := "88888888-8888-8888-8888-888888888888"
	mock.ExpectQuery(`FROM recall_context_log\s+WHERE context_id`).
		WithArgs(ctxID).
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "repo_id", "verb",
			"query_json", "reranker_model_version",
			"served_under_degraded", "created_at",
			"node_ids", "edge_ids", "concept_ids",
		}).AddRow(
			ctxID, testRepoID, "recall",
			`{}`, "rerank-v1",
			false, frozen,
			"{}", "{}", "{}",
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/context/"+ctxID))
	assertDegradedEnvelope(t, rec, reason)
	if got := metric.Count(VerbReadContext, reason); got != 1 {
		t.Fatalf("metric.Count(%q, %q) = %d, want 1", VerbReadContext, reason, got)
	}
}

func TestMgmtReadConcepts_healthDegraded_emitsClosedSetEnvelope(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	reason := degraded.ReasonGraphStoreUnavailable
	h, mock, cleanup := newPerVerbHealthHandler(t, frozen, healthSrcReason(reason), metric)
	defer cleanup()

	mock.ExpectQuery(`FROM concept c\s+LEFT JOIN LATERAL.*FROM concept_version`).
		WithArgs(false, false, readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "created_at",
			"version_index", "confidence", "confidence_band",
			"support_count", "negative_count", "promoted", "version_created_at",
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/concepts"))
	assertDegradedEnvelope(t, rec, reason)
	if got := metric.Count(VerbReadConcepts, reason); got != 1 {
		t.Fatalf("metric.Count(%q, %q) = %d, want 1", VerbReadConcepts, reason, got)
	}
}

func TestMgmtReadConceptSupports_healthDegraded_emitsClosedSetEnvelope(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	reason := degraded.ReasonGraphStoreUnavailable
	h, mock, cleanup := newPerVerbHealthHandler(t, frozen, healthSrcReason(reason), metric)
	defer cleanup()

	conceptID := "99999999-9999-9999-9999-999999999999"
	mock.ExpectQuery(`FROM concept_support`).
		WithArgs(conceptID, "", readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"support_id", "concept_id", "concept_version_id",
			"repo_id", "node_id", "episode_id",
			"polarity", "created_at",
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/concept_supports?concept_id="+conceptID))
	assertDegradedEnvelope(t, rec, reason)
	if got := metric.Count(VerbReadConceptSupports, reason); got != 1 {
		t.Fatalf("metric.Count(%q, %q) = %d, want 1", VerbReadConceptSupports, reason, got)
	}
}

func TestMgmtReadGraphNode_healthDegraded_emitsClosedSetEnvelope(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	reason := degraded.ReasonGraphStoreUnavailable
	h, mock, cleanup := newPerVerbHealthHandler(t, frozen, healthSrcReason(reason), metric)
	defer cleanup()

	nodeID := testRepoID
	mock.ExpectQuery(`FROM node n\s+LEFT JOIN node_retirement`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "kind", "canonical_signature", "from_sha",
			"parent_node_id", "attrs_json", "retired_at_sha",
		}).AddRow(
			testRepoID, "method", "Foo::bar()", testHeadSHA,
			"", `{}`, "",
		))
	mock.ExpectQuery(`FROM edge e\s+LEFT JOIN node neighbor ON neighbor\.node_id = e\.dst_node_id`).
		WithArgs(nodeID, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))
	mock.ExpectQuery(`FROM edge e\s+LEFT JOIN node neighbor ON neighbor\.node_id = e\.src_node_id`).
		WithArgs(nodeID, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/"+nodeID))
	assertDegradedEnvelope(t, rec, reason)
	if got := metric.Count(VerbReadGraphNode, reason); got != 1 {
		t.Fatalf("metric.Count(%q, %q) = %d, want 1", VerbReadGraphNode, reason, got)
	}
}

func TestMgmtReadTraceObservation_healthDegraded_emitsClosedSetEnvelope(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	metric := degraded.NewCounter()
	reason := degraded.ReasonSpanIngestorBackpressure
	h, mock, cleanup := newPerVerbHealthHandler(t, frozen, healthSrcReason(reason), metric)
	defer cleanup()

	edgeID := testRepoID
	mock.ExpectQuery(`FROM trace_observation\s+WHERE edge_id`).
		WithArgs(edgeID).
		WillReturnRows(sqlmock.NewRows([]string{
			"observation_count", "p50_latency_ms", "p95_latency_ms", "latest_span_ref", "last_observed_at",
		}).AddRow(int64(0), 0.0, 0.0, "", frozen))
	mock.ExpectQuery(`FROM trace_observation_log\s+WHERE edge_id`).
		WithArgs(edgeID, sqlmock.AnyArg(), false, sqlmock.AnyArg(), 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"span_log_id", "trace_id", "span_id", "started_at", "duration_ms",
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/trace_observation/"+edgeID))
	assertDegradedEnvelope(t, rec, reason)
	if got := metric.Count(VerbReadTraceObservation, reason); got != 1 {
		t.Fatalf("metric.Count(%q, %q) = %d, want 1", VerbReadTraceObservation, reason, got)
	}
}

// TestMgmtRead_globalHealth_appliesToGlobalScopeVerbs pins the
// iter-3 fix for evaluator finding #4: the mgmt.read verbs that
// pass an empty repoID to the HealthSource (observations,
// concepts, trace_observation — they read across repos)
// receive the same global probe result that the production
// `cmd/mgmt-api` global-health query surfaces. The
// `MgmtHealthSource` here returns the closed-set reason for
// every probe regardless of repoID, simulating the production
// adapter's "highest-priority degraded across the fleet"
// behavior. All three global-scope verbs must surface the
// reason on the wire envelope.
func TestMgmtRead_globalHealth_appliesToGlobalScopeVerbs(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		verb      string
		setupSQL  func(sqlmock.Sqlmock)
		url       string
		reason    string
	}{
		{
			name: "observations",
			verb: VerbReadObservations,
			setupSQL: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`SELECT created_at FROM episode WHERE episode_id`).
					WithArgs(testRepoID).
					WillReturnRows(sqlmock.NewRows([]string{"created_at"}).
						AddRow(frozen.Add(-time.Hour)))
				mock.ExpectQuery(`FROM observation\s+WHERE episode_id = .* AND created_at >=`).
					WithArgs(testRepoID, frozen.Add(-time.Hour)).
					WillReturnRows(sqlmock.NewRows([]string{
						"observation_id", "role", "node_id", "edge_id", "concept_id",
						"degraded_recall_context_id", "weight", "created_at",
					}))
			},
			url:    "/v1/observations?episode_id=" + testRepoID,
			reason: degraded.ReasonEpisodicLogUnavailable,
		},
		{
			name: "concepts",
			verb: VerbReadConcepts,
			setupSQL: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`FROM concept c\s+LEFT JOIN LATERAL.*FROM concept_version`).
					WithArgs(false, false, readDefaultLimit).
					WillReturnRows(sqlmock.NewRows([]string{
						"concept_id", "name", "description_md", "created_at",
						"version_index", "confidence", "confidence_band",
						"support_count", "negative_count", "promoted", "version_created_at",
					}))
			},
			url:    "/v1/concepts",
			reason: degraded.ReasonGraphStoreUnavailable,
		},
		{
			name: "trace_observation",
			verb: VerbReadTraceObservation,
			setupSQL: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(`FROM trace_observation\s+WHERE edge_id`).
					WithArgs(testRepoID).
					WillReturnRows(sqlmock.NewRows([]string{
						"observation_count", "p50_latency_ms", "p95_latency_ms",
						"latest_span_ref", "last_observed_at",
					}).AddRow(int64(0), 0.0, 0.0, "", frozen))
				mock.ExpectQuery(`FROM trace_observation_log\s+WHERE edge_id`).
					WithArgs(testRepoID, sqlmock.AnyArg(), false, sqlmock.AnyArg(), 0).
					WillReturnRows(sqlmock.NewRows([]string{
						"span_log_id", "trace_id", "span_id", "started_at", "duration_ms",
					}))
			},
			url:    "/v1/trace_observation/" + testRepoID,
			reason: degraded.ReasonSpanIngestorBackpressure,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			metric := degraded.NewCounter()
			// HealthSource asserts the verb's call passed empty
			// repoID — the iter-3 invariant.
			hs := MgmtHealthSourceFunc(func(_ context.Context, verb, repoID string) (string, error) {
				if repoID != "" {
					t.Errorf("verb %s called HealthSource with repoID=%q; want \"\" (global scope)",
						verb, repoID)
				}
				return c.reason, nil
			})
			h, mock, cleanup := newPerVerbHealthHandler(t, frozen, hs, metric)
			defer cleanup()
			c.setupSQL(mock)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, readReq(t, true, c.url))
			assertDegradedEnvelope(t, rec, c.reason)
			if got := metric.Count(c.verb, c.reason); got != 1 {
				t.Errorf("metric.Count(%q, %q) = %d, want 1", c.verb, c.reason, got)
			}
		})
	}
}
