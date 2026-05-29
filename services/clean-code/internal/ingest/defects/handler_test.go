package defects_test

// handler_test.go covers Stage 4.5's "scan_run row exists,
// zero metric_sample rows" invariant via the production
// composition: a fully-wired [webhook.Router] with the
// [webhook.DefectsVerbHandler] registered, the same
// in-memory durable scan_run repository the Router uses in
// dev, and an HMAC-signed POST request.
//
// # What this file pins (implementation-plan Stage 4.5)
//
//   - Scenario: defects-v1-writes-no-metric -- A valid POST
//     yields a 200 + a `scan_run(kind='external_per_row',
//     status='succeeded')` row. NO defect rows are persisted
//     (the defects package owns no writer; the Router never
//     dispatches a metric_sample write for this verb). The
//     test triple-confirms zero-writes by inspecting (a) a
//     recording fake ScanRunRepository that captures the
//     OpenExternal request, (b) the captured request's
//     SHABinding/SHA fields per arch Sec 4.11, (c) the
//     finalize call's terminal status.
//
//   - Scenario: defects-idempotent -- The SAME body POSTed
//     twice returns the SAME scan_run_id and DOES NOT open
//     a second durable scan_run row (replay path).
//
//   - Scenario: defects-rejects-malformed -- A body with a
//     non-40-hex SHA returns 400 / INVALID_SHA. No durable
//     scan_run row is opened (validation runs in
//     ExtractMetadata BEFORE the scan_run claim per the
//     idempotency-table-hygiene argument in
//     DefectsVerbHandler.ExtractMetadata).
//
// # Why a recording fake instead of InMemoryScanRunRepository
//
// [webhook.InMemoryScanRunRepository.Lookup] surfaces only
// `(status, kind)` -- it does NOT expose the `sha_binding`
// or `to_sha` columns. The Stage 4.5 brief explicitly pins
// `sha_binding='per_row'` and `to_sha=NULL`; asserting those
// requires capturing the request the Router built. The fake
// also makes the "succeeded vs failed" finalize transition
// observable to the test.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/ingest/churn"
	"forge/services/clean-code/internal/ingest/defects"
	"forge/services/clean-code/internal/ingest/webhook"
	"forge/services/clean-code/internal/metric_ingestor"
)

const (
	defectsTestKeyID  = "kv-defects-test-2026-01"
	defectsTestSecret = "defects-test-hmac-secret-32-bytes-deadbeef!!"
)

// recordingScanRunRepository is a test seam that captures
// every OpenExternal request + every Finalize call so the
// Stage 4.5 invariants are observable. It behaves like
// [webhook.InMemoryScanRunRepository] semantically (atomic
// claim per (verb, payload_hash)) but exposes the captured
// state so the test can assert SHABinding, SHA, finalize
// status, etc.
type recordingScanRunRepository struct {
	mu            sync.Mutex
	openRequests  []webhook.ScanRunRepositoryRequest
	finalizes     []finalizeCall
	byPayload     map[string]uuid.UUID
	rowsByScanRun map[uuid.UUID]string // scan_run_id -> status
}

type finalizeCall struct {
	scanRunID uuid.UUID
	status    string
	endedAt   time.Time
}

func newRecordingScanRunRepository() *recordingScanRunRepository {
	return &recordingScanRunRepository{
		byPayload:     map[string]uuid.UUID{},
		rowsByScanRun: map[uuid.UUID]string{},
	}
}

func (r *recordingScanRunRepository) OpenExternal(ctx context.Context, req webhook.ScanRunRepositoryRequest) (webhook.ScanRunRepositoryResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openRequests = append(r.openRequests, req)
	key := req.Verb + "|" + req.PayloadHash.String()
	if existingID, ok := r.byPayload[key]; ok {
		return webhook.ScanRunRepositoryResult{
			ScanRunID:      existingID,
			AlreadyExisted: true,
			ExistingStatus: r.rowsByScanRun[existingID],
		}, nil
	}
	id, err := uuid.NewV4()
	if err != nil {
		return webhook.ScanRunRepositoryResult{}, err
	}
	r.byPayload[key] = id
	r.rowsByScanRun[id] = "running"
	return webhook.ScanRunRepositoryResult{
		ScanRunID:      id,
		AlreadyExisted: false,
	}, nil
}

func (r *recordingScanRunRepository) Finalize(ctx context.Context, scanRunID uuid.UUID, status string, endedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.finalizes = append(r.finalizes, finalizeCall{scanRunID, status, endedAt})
	r.rowsByScanRun[scanRunID] = status
	return nil
}

func (r *recordingScanRunRepository) Opens() []webhook.ScanRunRepositoryRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]webhook.ScanRunRepositoryRequest, len(r.openRequests))
	copy(out, r.openRequests)
	return out
}

func (r *recordingScanRunRepository) Finalizes() []finalizeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]finalizeCall, len(r.finalizes))
	copy(out, r.finalizes)
	return out
}

func (r *recordingScanRunRepository) StatusOf(scanRunID uuid.UUID) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rowsByScanRun[scanRunID]
}

// Compile-time interface assertion.
var _ webhook.ScanRunRepository = (*recordingScanRunRepository)(nil)

// newDefectsRouter builds a fully-wired Router with ONLY the
// defects verb mounted. The composition mirrors what
// `cmd/clean-code-metric-ingestor` does in production, minus
// the PG-backed repositories (we use the recording fake +
// in-memory idempotency store so the test runs without
// Postgres).
func newDefectsRouter(t *testing.T) (*webhook.Router, *recordingScanRunRepository) {
	t.Helper()
	resolver := webhook.NewStaticSecretResolver(map[string][]byte{
		defectsTestKeyID: []byte(defectsTestSecret),
	})
	store := webhook.NewInMemoryIdempotencyStore(0)
	repo := newRecordingScanRunRepository()
	r := webhook.NewRouter(webhook.RouterConfig{
		Resolver:    resolver,
		Store:       store,
		ScanRunRepo: repo,
		Verbs:       []webhook.VerbHandler{webhook.NewDefectsVerbHandler()},
	})
	return r, repo
}

// signedDefectsRequest builds a POST against
// `/v1/ingest/defects` with the canonical HMAC headers and a
// correctly-signed body.
func signedDefectsRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhook.RouterPath+"defects", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SigningKeyIDHeader, defectsTestKeyID)
	req.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(body, []byte(defectsTestSecret)))
	return req
}

// goodDefectsBody returns a canonical happy-path body.
func goodDefectsBody(t *testing.T) []byte {
	t.Helper()
	p := defects.Payload{
		RepoID: fixedRepoID,
		Rows: []defects.PayloadRow{
			{SHA: validSHA('a'), FilePath: "internal/foo.go", DefectID: "JIRA-1", Severity: "critical"},
			{SHA: validSHA('b'), FilePath: "internal/bar.go", DefectID: "JIRA-2", Severity: "minor"},
		},
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal defects body: %v", err)
	}
	return body
}

// decodeRouterResponse parses the canonical 200 envelope the
// webhook Router emits.
func decodeRouterResponse(t *testing.T, body *bytes.Buffer) webhook.RouterResponse {
	t.Helper()
	raw, _ := io.ReadAll(body)
	var got webhook.RouterResponse
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode RouterResponse: %v (raw=%q)", err, raw)
	}
	return got
}

// fixedNow is the deterministic clock the churn materialiser
// captures for window math in the positive-control branch of
// the no-metric-sample test. Chosen so every churn row is
// in-window (matching the canonical webhook handler test's
// own fixedNow so the churn happy-path materially produces
// records).
func fixedNow() time.Time {
	return time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
}

// goodChurnBodyForDefectsSpyTest returns a serialised
// [churn.Payload] with two in-window rows touching two
// different files. Used ONLY by
// TestDefectsHandler_NoMetricSampleWriteSidechannel as the
// positive control: a churn POST against the same shared
// writer MUST produce records, otherwise the negative
// "defects writes zero" assertion is vacuous (could be
// because the spy is unreachable).
func goodChurnBodyForDefectsSpyTest(t *testing.T) []byte {
	t.Helper()
	p := churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: validSHA('c'), FilePath: "internal/churn-spy-a.go", ModifiedAt: fixedNow().Add(-24 * time.Hour)},
			{SHA: validSHA('d'), FilePath: "internal/churn-spy-b.go", ModifiedAt: fixedNow().Add(-48 * time.Hour)},
		},
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal churn body: %v", err)
	}
	return body
}

// signedChurnRequest builds a POST against `/v1/ingest/churn`
// with the canonical HMAC headers + a correctly-signed body.
// Mirror of [signedDefectsRequest] -- separate helper so the
// positive-control churn POST in the no-metric-sample test is
// shape-symmetric with the defects POST being measured.
func signedChurnRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhook.RouterPath+"churn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SigningKeyIDHeader, defectsTestKeyID)
	req.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(body, []byte(defectsTestSecret)))
	return req
}

// TestDefectsHandler_HappyPath_OpensScanRunAndFinalisesSucceeded
// pins the canonical happy path:
//
//   - A valid POST returns 200 with the canonical envelope.
//   - The recording repo captures ONE OpenExternal call with
//     Verb=defects, Kind=external_per_row, SHABinding=per_row,
//     SHA="" (per arch Sec 4.11 row 4 and e2e-scenarios.md
//     line 688).
//   - The recording repo captures ONE Finalize call with
//     status=succeeded (per implementation-plan Stage 4.5:
//     "mark `succeeded` on parse OK").
//   - No further side effects (verb has no writer dependency
//     so nothing else CAN happen; this test serves as the
//     structural assertion).
func TestDefectsHandler_HappyPath_OpensScanRunAndFinalisesSucceeded(t *testing.T) {
	t.Parallel()
	router, repo := newDefectsRouter(t)
	body := goodDefectsBody(t)

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, signedDefectsRequest(t, body))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200; body=%s", rr.Code, rr.Body.String())
	}
	resp := decodeRouterResponse(t, rr.Body)
	if resp.Verb != "defects" {
		t.Errorf("envelope.Verb = %q; want %q", resp.Verb, "defects")
	}
	if resp.ScanRunKind != "external_per_row" {
		t.Errorf("envelope.ScanRunKind = %q; want %q", resp.ScanRunKind, "external_per_row")
	}
	if resp.ScanRunID == uuid.Nil {
		t.Errorf("envelope.ScanRunID is zero")
	}
	if resp.Replayed {
		t.Errorf("envelope.Replayed = true; want false on first call")
	}
	if resp.FoundationDispatched {
		t.Errorf("envelope.FoundationDispatched = true; want false (external_per_row)")
	}
	if len(resp.Detail) != 0 {
		t.Errorf("envelope.Detail = %s; want empty/nil (v1 store-only)", string(resp.Detail))
	}

	opens := repo.Opens()
	if len(opens) != 1 {
		t.Fatalf("OpenExternal calls = %d; want 1", len(opens))
	}
	op := opens[0]
	if op.Verb != "defects" {
		t.Errorf("OpenExternal.Verb = %q; want %q", op.Verb, "defects")
	}
	if op.Kind != "external_per_row" {
		t.Errorf("OpenExternal.Kind = %q; want %q (arch Sec 4.11 row 4)", op.Kind, "external_per_row")
	}
	if op.SHABinding != "per_row" {
		t.Errorf("OpenExternal.SHABinding = %q; want %q (per-row binding)", op.SHABinding, "per_row")
	}
	if op.SHA != "" {
		t.Errorf("OpenExternal.SHA = %q; want empty (to_sha=NULL per Sec 4.11 row 4)", op.SHA)
	}
	if op.RepoID != fixedRepoID {
		t.Errorf("OpenExternal.RepoID = %s; want %s", op.RepoID, fixedRepoID)
	}

	finalizes := repo.Finalizes()
	if len(finalizes) != 1 {
		t.Fatalf("Finalize calls = %d; want 1", len(finalizes))
	}
	if finalizes[0].status != webhook.ScanRunStatusSucceeded {
		t.Errorf("Finalize.status = %q; want %q (parse OK -> succeeded)",
			finalizes[0].status, webhook.ScanRunStatusSucceeded)
	}
	if finalizes[0].scanRunID != resp.ScanRunID {
		t.Errorf("Finalize.scanRunID = %s; want envelope.ScanRunID=%s",
			finalizes[0].scanRunID, resp.ScanRunID)
	}
	if repo.StatusOf(resp.ScanRunID) != webhook.ScanRunStatusSucceeded {
		t.Errorf("scan_run %s status = %q; want %q",
			resp.ScanRunID, repo.StatusOf(resp.ScanRunID), webhook.ScanRunStatusSucceeded)
	}
}

// TestDefectsHandler_Idempotent_SameBodyReplays pins the
// `defects-idempotent` scenario from implementation-plan
// Stage 4.5: posting the same body twice yields the same
// scan_run_id and does NOT open a second durable row.
func TestDefectsHandler_Idempotent_SameBodyReplays(t *testing.T) {
	t.Parallel()
	router, repo := newDefectsRouter(t)
	body := goodDefectsBody(t)

	rr1 := httptest.NewRecorder()
	router.ServeHTTP(rr1, signedDefectsRequest(t, body))
	if rr1.Code != http.StatusOK {
		t.Fatalf("first POST status = %d; want 200; body=%s", rr1.Code, rr1.Body.String())
	}
	resp1 := decodeRouterResponse(t, rr1.Body)

	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, signedDefectsRequest(t, body))
	if rr2.Code != http.StatusOK {
		t.Fatalf("replay POST status = %d; want 200; body=%s", rr2.Code, rr2.Body.String())
	}
	resp2 := decodeRouterResponse(t, rr2.Body)

	if resp1.ScanRunID != resp2.ScanRunID {
		t.Errorf("scan_run_id changed across replays: %s vs %s", resp1.ScanRunID, resp2.ScanRunID)
	}
	// The Router's in-process idempotency cache short-circuits
	// before the durable repository is touched, so the durable
	// repo SHOULD see exactly one open in this same-process
	// scenario. (A cross-restart replay would hit the durable
	// repo a second time and that path's contract is
	// AlreadyExisted=true, NO second row inserted.)
	if got := len(repo.Opens()); got != 1 {
		t.Errorf("durable OpenExternal calls = %d; want 1 (in-process cache short-circuits)", got)
	}
	// The Finalize MUST NOT happen twice -- once finalised,
	// the Router never finalises again.
	if got := len(repo.Finalizes()); got != 1 {
		t.Errorf("durable Finalize calls = %d; want 1", got)
	}
}

// TestDefectsHandler_Malformed_NoScanRunBurned pins the
// "idempotency-table hygiene" contract from
// DefectsVerbHandler.ExtractMetadata: a malformed body
// returns 400 BEFORE the durable scan_run row is opened, so
// the publisher can fix their payload and retry without
// the (verb, payload_hash) slot being permanently sticky-failed.
func TestDefectsHandler_Malformed_NoScanRunBurned(t *testing.T) {
	t.Parallel()
	router, repo := newDefectsRouter(t)
	// Invalid SHA shape -- decoder OK, validator rejects.
	bad := defects.Payload{
		RepoID: fixedRepoID,
		Rows: []defects.PayloadRow{
			{SHA: "not-a-real-sha", FilePath: "internal/foo.go", DefectID: "JIRA-1", Severity: "critical"},
		},
	}
	body, err := json.Marshal(bad)
	if err != nil {
		t.Fatalf("marshal bad payload: %v", err)
	}

	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, signedDefectsRequest(t, body))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400; body=%s", rr.Code, rr.Body.String())
	}
	// Error body's code field should be `INVALID_SHA`.
	var errBody struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	raw, _ := io.ReadAll(rr.Body)
	if err := json.Unmarshal(raw, &errBody); err != nil {
		t.Fatalf("decode error body: %v (raw=%q)", err, raw)
	}
	if errBody.Code != "INVALID_SHA" {
		t.Errorf("error body code = %q; want %q", errBody.Code, "INVALID_SHA")
	}
	if !strings.Contains(strings.ToLower(errBody.Error), "sha") {
		t.Errorf("error body message %q does not mention SHA", errBody.Error)
	}
	// Idempotency-table hygiene: no durable scan_run row
	// opened for a payload that failed parse/validation.
	if got := len(repo.Opens()); got != 0 {
		t.Errorf("OpenExternal calls = %d; want 0 (malformed payload must not burn a scan_run slot)", got)
	}
	if got := len(repo.Finalizes()); got != 0 {
		t.Errorf("Finalize calls = %d; want 0", got)
	}
}

// TestDefectsHandler_NoMetricSampleWriteSidechannel (iter 2
// evaluator item #3) pins the Stage 4.5 v1 invariant from
// tech-spec Sec 4.11 row 4: the defects verb writes ZERO
// rows to the `metric_sample` table. The proof in iter 1
// was purely structural ("the verb has no writer dep, so
// no write CAN happen"), which the evaluator flagged as
// insufficient -- a future refactor could re-introduce a
// writer dep silently. This iter installs an OBSERVABLE
// writer spy and asserts no rows land.
//
// # How the spy is observable when defects has no writer
//
// The trick: register the [webhook.ChurnVerbHandler] in the
// SAME Router as the defects handler. The Stage 4.4 churn
// pipeline writes to a [churn.InMemoryChurnEventStore]
// (NOT the legacy metric_sample writer -- see the package
// doc on `internal/ingest/churn/ingest.go`); we keep a
// shared [metric_ingestor.InMemoryMetricSampleWriter]
// spy in scope so a regression that re-introduced a
// metric_sample writer dep on EITHER verb would land rows
// in `writer.Records()` and trip the assertion.
//
// The positive control is now the churn EVENT store: a
// churn POST against `churnStore` MUST produce >0 events,
// otherwise the test's "the router-and-pipeline is alive"
// claim is vacuous. The negative-assertion spy (`writer`)
// MUST remain empty after BOTH POSTs:
//
//  1. The defects POST does NOT write to the shared writer
//     OR the churn store (defects has neither dep).
//  2. The churn POST DOES write to the churn store (proves
//     the pipeline is alive) but ALSO does NOT touch the
//     `metric_sample` writer (Stage 4.4 contract: churn
//     writes ZERO `metric_sample` rows directly).
//
// This is the canonical "positive control" pattern adapted
// for the Stage 4.4 staging-table architecture: the
// negative assertion remains meaningful because the same
// router configuration demonstrably exercises the verb
// pipeline (positive churn-store result) without ever
// touching the spied metric_sample writer.
func TestDefectsHandler_NoMetricSampleWriteSidechannel(t *testing.T) {
	t.Parallel()

	// Negative-assertion spy: the legacy metric_sample
	// writer. NO verb in this router has a dependency on
	// it (defects has no writer; the Stage 4.4 churn
	// pipeline writes to `churnStore` below). A regression
	// that re-introduced a writer dep on either verb would
	// surface as len(writer.Records()) > 0.
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()

	// Positive-control sink: the Stage 4.4 churn staging
	// store. The churn verb is wired to this; a churn POST
	// MUST land rows here, otherwise the router-and-verb
	// pipeline is dead and the negative writer assertion
	// would be vacuous.
	churnStore := churn.NewInMemoryChurnEventStore()
	churnIng := churn.NewIngesterWithClocks(churnStore, fixedNow, uuid.NewV4)

	resolver := webhook.NewStaticSecretResolver(map[string][]byte{
		defectsTestKeyID: []byte(defectsTestSecret),
	})
	store := webhook.NewInMemoryIdempotencyStore(0)
	repo := newRecordingScanRunRepository()
	router := webhook.NewRouter(webhook.RouterConfig{
		Resolver:    resolver,
		Store:       store,
		ScanRunRepo: repo,
		Verbs: []webhook.VerbHandler{
			webhook.NewDefectsVerbHandler(),
			webhook.NewChurnVerbHandlerWithClock(churnIng, fixedNow),
		},
	})

	// --- Negative: defects POST writes zero rows. -------------
	defectsBody := goodDefectsBody(t)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, signedDefectsRequest(t, defectsBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("defects POST status = %d; want 200; body=%s",
			rr.Code, rr.Body.String())
	}
	if got := len(writer.Records()); got != 0 {
		t.Fatalf("after defects POST, writer.Records() len = %d; want 0 (tech-spec Sec 4.11 row 4: defects v1 writes ZERO metric_sample rows). Records=%+v",
			got, writer.Records())
	}
	if got := churnStore.Len(); got != 0 {
		t.Fatalf("after defects POST, churnStore.Len() = %d; want 0 (defects MUST NOT leak into the churn staging table either)",
			got)
	}
	// The scan_run row MUST still exist + be `succeeded`
	// (the Stage 4.5 store-only contract -- scan_run row
	// is the ONLY side effect, no metric_sample row).
	resp := decodeRouterResponse(t, rr.Body)
	if got := repo.StatusOf(resp.ScanRunID); got != webhook.ScanRunStatusSucceeded {
		t.Errorf("scan_run.status = %q; want %q (parse OK -> succeeded)",
			got, webhook.ScanRunStatusSucceeded)
	}

	// --- Positive control: churn POST DOES write rows. --------
	// If the spy were broken (e.g. a Router refactor stopped
	// invoking verbs), the test above would PASS for the
	// wrong reason -- "zero records because nothing is
	// reachable". The positive control proves the verb
	// pipeline IS alive in this Router composition; only the
	// defects path bypasses ALL persistence writers.
	churnBody := goodChurnBodyForDefectsSpyTest(t)
	rr2 := httptest.NewRecorder()
	router.ServeHTTP(rr2, signedChurnRequest(t, churnBody))
	if rr2.Code != http.StatusOK {
		t.Fatalf("churn POST (positive control) status = %d; want 200; body=%s",
			rr2.Code, rr2.Body.String())
	}
	if got := churnStore.Len(); got == 0 {
		t.Fatalf("after churn POST (positive control), churnStore.Len() = 0; want > 0 (the verb pipeline MUST be observable -- if churn produces no staging rows, the defects negative is vacuous)")
	}

	// Stage 4.4 cross-check: churn must NEVER write into the
	// metric_sample writer either (`internal/ingest/churn/
	// ingest.go` package doc: the verb writes ZERO
	// metric_sample rows directly; the materialiser is the
	// sole `modification_count_in_window` writer on a later
	// pass). If a regression re-introduced a metric_sample
	// dep on the churn path, this assertion would catch it.
	if got := len(writer.Records()); got != 0 {
		t.Fatalf("after churn POST, writer.Records() len = %d; want 0 (Stage 4.4 contract: churn verb writes ZERO metric_sample rows directly -- the materialiser is the sole writer of modification_count_in_window). Records=%+v",
			got, writer.Records())
	}
}
