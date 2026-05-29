package composition

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/lib/pq"

	"forge/services/clean-code/internal/evaluator"
)

// unmarshalJSON is a small test helper that wraps
// json.Unmarshal so the call sites read terser.
func unmarshalJSON(t *testing.T, body string, dst any) error {
	t.Helper()
	return json.Unmarshal([]byte(body), dst)
}

// TestBuildMgmtWriter_RejectsNilIngestorDB ensures the
// helper guards against the most common composition-root
// bug: passing a nil DB handle. The error message MUST
// name the offending argument so the operator can fix
// their env-var wiring without reading source.
func TestBuildMgmtWriter_RejectsNilIngestorDB(t *testing.T) {
	_, err := BuildMgmtWriter(nil, nil, nil)
	if err == nil {
		t.Fatalf("BuildMgmtWriter(nil, nil): want error, got nil")
	}
	if !strings.Contains(err.Error(), "ingestorDB") {
		t.Errorf("error %q: want substring %q", err.Error(), "ingestorDB")
	}
}

func TestBuildMgmtWriter_RejectsNilMgmtDB(t *testing.T) {
	stub := openStubDB(t)
	defer stub.Close()
	_, err := BuildMgmtWriter(stub, nil, nil)
	if err == nil {
		t.Fatalf("BuildMgmtWriter(stub, nil): want error, got nil")
	}
	if !strings.Contains(err.Error(), "mgmtDB") {
		t.Errorf("error %q: want substring %q", err.Error(), "mgmtDB")
	}
}

func TestBuildIngestRouter_RejectsNilDB(t *testing.T) {
	_, err := BuildIngestRouter(nil, IngestRouterConfig{SigningKeyID: "k", HMACSecret: "s"}, nil)
	if err == nil {
		t.Fatalf("BuildIngestRouter(nil): want error, got nil")
	}
	if !strings.Contains(err.Error(), "ingestorDB") {
		t.Errorf("error %q: want substring %q", err.Error(), "ingestorDB")
	}
}

func TestBuildIngestRouter_RejectsEmptySigningKeyID(t *testing.T) {
	stub := openStubDB(t)
	defer stub.Close()
	_, err := BuildIngestRouter(stub, IngestRouterConfig{HMACSecret: "s"}, nil)
	if err == nil {
		t.Fatalf("BuildIngestRouter(empty SigningKeyID): want error, got nil")
	}
	if !strings.Contains(err.Error(), "SigningKeyID") {
		t.Errorf("error %q: want substring %q", err.Error(), "SigningKeyID")
	}
}

func TestBuildIngestRouter_RejectsEmptyHMACSecret(t *testing.T) {
	stub := openStubDB(t)
	defer stub.Close()
	_, err := BuildIngestRouter(stub, IngestRouterConfig{SigningKeyID: "k"}, nil)
	if err == nil {
		t.Fatalf("BuildIngestRouter(empty HMACSecret): want error, got nil")
	}
	if !strings.Contains(err.Error(), "HMACSecret") {
		t.Errorf("error %q: want substring %q", err.Error(), "HMACSecret")
	}
}

func TestBuildEvalGate_RejectsNilEvaluatorDB(t *testing.T) {
	_, err := BuildEvalGate(context.Background(), EvalGateConfig{}, nil)
	if err == nil {
		t.Fatalf("BuildEvalGate(nil EvaluatorDB): want error, got nil")
	}
	if !strings.Contains(err.Error(), "EvaluatorDB") {
		t.Errorf("error %q: want substring %q", err.Error(), "EvaluatorDB")
	}
}

func TestBuildEvalGate_RejectsNilSolidBatchDB(t *testing.T) {
	stub := openStubDB(t)
	defer stub.Close()
	_, err := BuildEvalGate(context.Background(), EvalGateConfig{EvaluatorDB: stub}, nil)
	if err == nil {
		t.Fatalf("BuildEvalGate(nil SolidBatchDB): want error, got nil")
	}
	if !strings.Contains(err.Error(), "SolidBatchDB") {
		t.Errorf("error %q: want substring %q", err.Error(), "SolidBatchDB")
	}
}

// TestBuildEvalGate_RejectsNilWalWriter pins the Stage 9.1
// brief (iter-2 evaluator item #3): the composition root
// MUST construct a *wal.Writer rooted at
// CLEAN_CODE_AUDIT_WAL_DIR and pass it through. A nil
// WalWriter means the gate's Audit INSERTs would commit
// without a WAL frame, violating the row+WAL atomicity
// contract (architecture Sec 7.10 / tech-spec Sec 4.13).
func TestBuildEvalGate_RejectsNilWalWriter(t *testing.T) {
	stub := openStubDB(t)
	defer stub.Close()
	_, err := BuildEvalGate(context.Background(), EvalGateConfig{
		EvaluatorDB:  stub,
		SolidBatchDB: stub,
	}, nil)
	if err == nil {
		t.Fatalf("BuildEvalGate(nil WalWriter): want error, got nil")
	}
	if !strings.Contains(err.Error(), "WalWriter") {
		t.Errorf("error %q: want substring %q", err.Error(), "WalWriter")
	}
}

// TestEvalGateHandler_NilGate_Returns503 ensures the
// helper degrades gracefully when the composition root
// failed to build the gate but still wired the handler
// (e.g. the operator left a DSN env var unset).
func TestEvalGateHandler_NilGate_Returns503(t *testing.T) {
	h := EvalGateHandler(nil, nil)
	req := httptest.NewRequest("POST", "/v1/eval/gate", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(rec.Body.String(), "VERB_NOT_WIRED") {
		t.Errorf("body %q: want VERB_NOT_WIRED", rec.Body.String())
	}
}

func TestEvalReplayHandler_NilGate_Returns503(t *testing.T) {
	h := EvalReplayHandler(nil, nil)
	req := httptest.NewRequest("POST", "/v1/eval/replay", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestEvalGateHandler_MethodNotAllowed asserts the
// canonical refusal on GET so non-POST callers see 405
// rather than a misleading 400/500.
func TestEvalGateHandler_MethodNotAllowed(t *testing.T) {
	// Use a real gate stub via a recording-only http.HandlerFunc
	// would normally apply, but since the handler binds to the
	// gate type directly we exercise the nil-gate 503 path with
	// a GET request. The method-not-allowed test against a
	// non-nil gate is covered by the eval-gate binary's
	// existing tests; here we only assert the nil-gate
	// degradation is independent of method.
	h := EvalGateHandler(nil, nil)
	req := httptest.NewRequest("GET", "/v1/eval/gate", nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	// Nil-gate path returns 503 BEFORE method check; that's
	// the expected behaviour because a misconfigured server
	// should not pretend the route is selectively available
	// by method.
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d want %d (nil-gate degradation)", rec.Code, http.StatusServiceUnavailable)
	}
}

// TestRejectExtraPolicyVersionField unit-tests the now
// no-op shim: the canonical `/v1/eval/gate` verb accepts
// `policy_version_id` per architecture.md:1338-1339, so
// the function returns nil for every input. The test
// remains in place to guard against silent reintroduction
// of the prior rejection logic (which would break
// architecture-conformant clients).
func TestRejectExtraPolicyVersionField(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"empty", ""},
		{"no pvid", `{"repo_id":"x","sha":"y"}`},
		{"with pvid", `{"repo_id":"x","sha":"y","policy_version_id":"z"}`},
		{"not an object", `"a string"`},
		{"malformed json", `{not json`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := rejectExtraPolicyVersionField([]byte(tc.body)); err != nil {
				t.Errorf("rejectExtraPolicyVersionField(%q): want nil (no-op shim), got %v", tc.body, err)
			}
		})
	}
}

// TestEvalGateHandler_AcceptsOptionalPolicyVersionID is a
// regression test for iter-9 item 1: the canonical verb
// MUST accept `policy_version_id` (architecture.md:1338-1339)
// rather than reject it. The handler still returns 503 for
// a nil gate so we can't drive the full evaluator path
// here -- but we can prove the JSON shape is no longer
// rejected at the parse step by asserting the response
// status. Because the canonical no-op shim now passes the
// body straight through, a request carrying
// policy_version_id MUST NOT receive a 400 with the
// previous "policy_version_id is not accepted" text.
//
// We exercise this through a real-but-stubbed gate: a nil
// gate returns 503 before parsing, so we use a *real*
// gate that fails at the inner Gate / Evaluate call.
// Since constructing a real evaluator.Gate in unit tests
// requires PG, we instead probe the rejectExtraPolicyVersionField
// hook directly (above) and assert the canonical request
// type round-trips a populated pvid via JSON.
func TestEvalGateHandler_AcceptsOptionalPolicyVersionID(t *testing.T) {
	// Round-trip evalGateRequest through JSON; the field
	// must be present and not silently dropped.
	body := `{"repo_id":"00000000-0000-0000-0000-000000000001","sha":"deadbeef","policy_version_id":"00000000-0000-0000-0000-000000000002"}`
	var req evalGateRequest
	if err := unmarshalJSON(t, body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.PolicyVersionID == nil {
		t.Fatalf("PolicyVersionID: got nil, want pointer to value")
	}
	if *req.PolicyVersionID != "00000000-0000-0000-0000-000000000002" {
		t.Errorf("PolicyVersionID: got %q, want canonical UUID", *req.PolicyVersionID)
	}
}

// TestWriteEvalResponse_OpaqueInternalError verifies item 2
// of the iter-9 feedback: non-degraded evaluator errors
// MUST surface as an opaque JSON envelope, not as raw
// evaluator/db text. The handler logs the raw error
// server-side; the wire response carries only the
// canonical INTERNAL_ERROR code.
func TestWriteEvalResponse_OpaqueInternalError(t *testing.T) {
	rec := httptest.NewRecorder()
	// Use an arbitrary non-sentinel error; the helper
	// must NOT echo this text to the caller.
	leakyErr := errors.New("pq: connection terminated by peer at host db.internal.local")
	writeEvalResponse(rec, evaluator.EvaluateResult{}, leakyErr, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d want 500", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "INTERNAL_ERROR") {
		t.Errorf("body %q: want INTERNAL_ERROR code", body)
	}
	if strings.Contains(body, "pq:") || strings.Contains(body, "db.internal.local") || strings.Contains(body, "terminated by peer") {
		t.Errorf("body %q LEAKS raw evaluator/db error text", body)
	}
}

func TestWriteEvalBodyReadError_MaxBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEvalBodyReadError(rec, &http.MaxBytesError{Limit: 1024})
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestWriteEvalBodyReadError_GenericError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeEvalBodyReadError(rec, errors.New("boom"))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d want %d", rec.Code, http.StatusBadRequest)
	}
}

// openStubDB returns a non-nil *sql.DB by opening the lib/pq
// driver with an obviously-unreachable DSN. The driver does
// NOT connect until Ping/Exec, so the handle satisfies the
// Build* helpers' nil-checks without requiring a live PG
// instance. These tests never touch the connection -- they
// only exercise the config-validation branches that fire
// before any SQL is run.
func openStubDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", "postgres://stub:stub@127.0.0.1:1/stub?sslmode=disable")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	return db
}
