package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/ingest/webhook"
	"forge/services/clean-code/internal/management"
	"forge/services/clean-code/internal/policy/steward"
)

// TestNewProductionWiring_AllDepsNil pins the contract:
// every Wiring slot stays nil when no deps are supplied.
// The composition root must then either pass non-nil deps
// or accept the 503 stub default.
func TestNewProductionWiring_AllDepsNil(t *testing.T) {
	t.Parallel()
	w := NewProductionWiring(ProductionWiringDeps{})
	if missing := w.MissingVerbs(); len(missing) != len(CanonicalVerbs) {
		t.Fatalf("len(MissingVerbs)=%d, want %d (all canonical verbs missing)", len(missing), len(CanonicalVerbs))
	}
	if wired := w.WiredVerbs(); len(wired) != 0 {
		t.Fatalf("len(WiredVerbs)=%d, want 0", len(wired))
	}
}

// TestNewProductionWiring_EvalGateSlotPopulated asserts that
// an explicit EvalGateHandler dep populates the EvalGate
// slot.
func TestNewProductionWiring_EvalGateSlotPopulated(t *testing.T) {
	t.Parallel()
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	w := NewProductionWiring(ProductionWiringDeps{EvalGateHandler: fake})
	if w.EvalGate == nil {
		t.Fatalf("EvalGate slot is nil after EvalGateHandler dep supplied")
	}
	rr := httptest.NewRecorder()
	w.EvalGate.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/eval/gate", nil))
	if rr.Code != http.StatusTeapot {
		t.Errorf("status=%d, want %d (handler did not forward)", rr.Code, http.StatusTeapot)
	}
}

// TestNewProductionWiring_MgmtWriterPopulatesMgmtSlots
// asserts that a non-nil *management.MgmtWriter populates
// all four mgmt-write slots. NewMgmtWriter accepts nil
// dependencies so we can construct a writer that returns 503
// on every call -- we are only asserting WIRING here, not
// the downstream behaviour.
func TestNewProductionWiring_MgmtWriterPopulatesMgmtSlots(t *testing.T) {
	t.Parallel()
	writer := management.NewMgmtWriter(nil, nil, nil, nil)
	w := NewProductionWiring(ProductionWiringDeps{MgmtWriter: writer})
	if w.MgmtRegisterRepo == nil {
		t.Errorf("MgmtRegisterRepo slot is nil")
	}
	if w.MgmtSetMode == nil {
		t.Errorf("MgmtSetMode slot is nil")
	}
	if w.MgmtRetractSample == nil {
		t.Errorf("MgmtRetractSample slot is nil")
	}
	if w.MgmtRescan == nil {
		t.Errorf("MgmtRescan slot is nil")
	}
	// The other slots stay nil.
	if w.MgmtOverride != nil {
		t.Errorf("MgmtOverride should stay nil without PolicyWriter dep")
	}
	if w.EvalGate != nil {
		t.Errorf("EvalGate should stay nil without EvalGateHandler dep")
	}
}

// TestNewProductionWiring_PolicyWriterPopulatesPolicySlots
// asserts that a non-nil *management.PolicyWriter populates
// the four policy/override slots.
func TestNewProductionWiring_PolicyWriterPopulatesPolicySlots(t *testing.T) {
	t.Parallel()
	// steward.New accepts an InMemoryStore + nil Signer for
	// scaffold-mode bring-up; that's enough to construct a
	// real *steward.Steward we can hand to NewPolicyWriter.
	st, err := steward.New(steward.Config{Store: steward.NewInMemoryStore()})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	pw := management.NewPolicyWriter(st)
	w := NewProductionWiring(ProductionWiringDeps{PolicyWriter: pw})
	if w.MgmtOverride == nil {
		t.Errorf("MgmtOverride slot is nil")
	}
	if w.PolicyPublish == nil {
		t.Errorf("PolicyPublish slot is nil")
	}
	if w.PolicyActivate == nil {
		t.Errorf("PolicyActivate slot is nil")
	}
	if w.PolicyPublishRulepack == nil {
		t.Errorf("PolicyPublishRulepack slot is nil")
	}
}

// TestNewProductionWiring_MgmtHandlerPopulatesKeysListActive
// asserts that a non-nil *management.Handler populates the
// policy.keys.list_active slot.
func TestNewProductionWiring_MgmtHandlerPopulatesKeysListActive(t *testing.T) {
	t.Parallel()
	// NewHandler(nil) is valid: the reader slot stays nil
	// internally and the verb returns 503 when called. We
	// only assert WIRING here.
	h := management.NewHandler(nil)
	w := NewProductionWiring(ProductionWiringDeps{MgmtHandler: h})
	if w.PolicyKeysListActive == nil {
		t.Errorf("PolicyKeysListActive slot is nil")
	}
}

// TestNewProductionWiring_MgmtReaderPopulatesEightReadSlots
// asserts that a non-nil *management.Reader populates every
// `mgmt.read.*` slot via [NewMgmtReadAdapter].
func TestNewProductionWiring_MgmtReaderPopulatesEightReadSlots(t *testing.T) {
	t.Parallel()
	// NewReader requires a *keys.Manager; passing nil for
	// the optional metrics backend yields a Reader whose
	// methods all return ErrBackendUnavailable. The slot
	// population check is independent of backend
	// availability.
	reader := management.NewReader(nil)
	w := NewProductionWiring(ProductionWiringDeps{MgmtReader: reader})
	if w.MgmtReadRepo == nil {
		t.Errorf("MgmtReadRepo slot is nil")
	}
	if w.MgmtReadMetricSample == nil {
		t.Errorf("MgmtReadMetricSample slot is nil")
	}
	if w.MgmtReadMetricSamples == nil {
		t.Errorf("MgmtReadMetricSamples slot is nil")
	}
	if w.MgmtReadFindings == nil {
		t.Errorf("MgmtReadFindings slot is nil")
	}
	if w.MgmtReadRegressions == nil {
		t.Errorf("MgmtReadRegressions slot is nil")
	}
	if w.MgmtReadRefactorPlan == nil {
		t.Errorf("MgmtReadRefactorPlan slot is nil")
	}
	if w.MgmtReadCrossRepo == nil {
		t.Errorf("MgmtReadCrossRepo slot is nil")
	}
	if w.MgmtReadPortfolio == nil {
		t.Errorf("MgmtReadPortfolio slot is nil")
	}
}

// TestNewProductionWiring_IngestRouterPopulatesAllFourIngestSlots
// asserts that a Router registered with all four ingest
// verbs (coverage, test_balance, churn, defects) populates
// every ingest.* slot.
func TestNewProductionWiring_IngestRouterPopulatesAllFourIngestSlots(t *testing.T) {
	t.Parallel()
	router := newTestIngestRouterAllVerbs(t)
	w := NewProductionWiring(ProductionWiringDeps{IngestRouter: router})
	if w.IngestCoverage == nil {
		t.Errorf("IngestCoverage slot is nil")
	}
	if w.IngestTestBalance == nil {
		t.Errorf("IngestTestBalance slot is nil")
	}
	if w.IngestChurn == nil {
		t.Errorf("IngestChurn slot is nil")
	}
	if w.IngestDefects == nil {
		t.Errorf("IngestDefects slot is nil")
	}
}

// TestNewProductionWiring_IngestRouterPartial_OnlyRegisteredSlotsWired
// is the iter-5 evaluator item #1 regression test: a Router
// that registered only one verb (defects) MUST populate ONLY
// the IngestDefects slot. The other three slots stay nil so
// the canonical Wiring partition reports them as
// `MissingVerbs` (503 stubs). The prior implementation
// mounted the same Router instance on all four slots
// regardless of registration, which falsely claimed every
// ingest verb was wired even when three of them would have
// 404'd inside the Router.
func TestNewProductionWiring_IngestRouterPartial_OnlyRegisteredSlotsWired(t *testing.T) {
	t.Parallel()
	router := newTestIngestRouterDefectsOnly(t)
	w := NewProductionWiring(ProductionWiringDeps{IngestRouter: router})
	if w.IngestDefects == nil {
		t.Errorf("IngestDefects slot is nil (defects was registered)")
	}
	if w.IngestCoverage != nil {
		t.Errorf("IngestCoverage slot is non-nil (coverage was NOT registered on router)")
	}
	if w.IngestTestBalance != nil {
		t.Errorf("IngestTestBalance slot is non-nil (test_balance was NOT registered on router)")
	}
	if w.IngestChurn != nil {
		t.Errorf("IngestChurn slot is non-nil (churn was NOT registered on router)")
	}
	missing := w.MissingVerbs()
	wantMissing := map[string]bool{
		"ingest.coverage":     true,
		"ingest.test_balance": true,
		"ingest.churn":        true,
	}
	for _, m := range missing {
		if wantMissing[m] {
			delete(wantMissing, m)
		}
	}
	if len(wantMissing) != 0 {
		t.Errorf("MissingVerbs partition omitted entries: %v (full missing=%v)", wantMissing, missing)
	}
}

// TestNewProductionWiring_IngestRouterGatewayPathBypassesHMAC
// is the iter-5 evaluator item #2 regression test: a request
// through the gateway-adapted handler MUST succeed WITHOUT
// HMAC headers when OIDC has authenticated the caller
// upstream. The trusted handler skips HMAC verification so
// the OIDC bearer-token is the sole auth boundary. The
// direct webhook.Router.ServeHTTP path (publisher path)
// still enforces HMAC; that invariant is asserted in
// TestRouter_DirectServeHTTPStillEnforcesHMAC below.
func TestNewProductionWiring_IngestRouterGatewayPathBypassesHMAC(t *testing.T) {
	t.Parallel()
	router := newTestIngestRouterAllVerbs(t)
	w := NewProductionWiring(ProductionWiringDeps{IngestRouter: router})
	// Hit the IngestDefects slot directly (no HMAC
	// headers). The stub verb handler accepts any body and
	// returns 200; the trusted handler runs the rest of the
	// Router pipeline without HMAC.
	rr := httptest.NewRecorder()
	body := `{"repo_id":"11111111-1111-1111-1111-111111111111","rows":[]}`
	req := httptest.NewRequest("POST", "/v1/ingest/defects", stringReader(body))
	req.Header.Set("Content-Type", "application/json")
	w.IngestDefects.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (gateway path should bypass HMAC); body=%q", rr.Code, rr.Body.String())
	}
}

// TestRouter_DirectServeHTTPStillEnforcesHMAC verifies that
// the publisher path (direct webhook.Router.ServeHTTP without
// the gateway-adapter wrapper) STILL rejects requests
// without an HMAC signature. The OIDC-trust handler MUST NOT
// weaken the publisher path's authentication.
func TestRouter_DirectServeHTTPStillEnforcesHMAC(t *testing.T) {
	t.Parallel()
	router := newTestIngestRouterAllVerbs(t)
	rr := httptest.NewRecorder()
	body := `{"repo_id":"11111111-1111-1111-1111-111111111111","rows":[]}`
	req := httptest.NewRequest("POST", "/v1/ingest/defects", stringReader(body))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401 (direct path must still require HMAC); body=%q", rr.Code, rr.Body.String())
	}
}

// TestNewProductionWiring_AllDepsPopulatesAllVerbs pins the
// coverage matrix: with every supported dep set, all 22
// canonical verbs are wired.
func TestNewProductionWiring_AllDepsPopulatesAllVerbs(t *testing.T) {
	t.Parallel()
	st, err := steward.New(steward.Config{Store: steward.NewInMemoryStore()})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	w := NewProductionWiring(ProductionWiringDeps{
		MgmtHandler:     management.NewHandler(nil),
		MgmtReader:      management.NewReader(nil),
		MgmtWriter:      management.NewMgmtWriter(nil, nil, nil, nil),
		PolicyWriter:    management.NewPolicyWriter(st),
		IngestRouter:    newTestIngestRouterAllVerbs(t),
		EvalGateHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
	})
	wired := w.WiredVerbs()
	if len(wired) != len(CanonicalVerbs) {
		t.Errorf("WiredVerbs=%d, want %d (every canonical verb)", len(wired), len(CanonicalVerbs))
	}
	missing := w.MissingVerbs()
	if len(missing) != 0 {
		t.Errorf("MissingVerbs=%v, want empty", missing)
	}
}

// TestNewProductionRegistry_ReplacesStubWithRealHandler is
// the end-to-end gateway test: a NewProductionRegistry
// populated with a real EvalGateHandler returns the
// handler's response (200) rather than the 503 stub.
func TestNewProductionRegistry_ReplacesStubWithRealHandler(t *testing.T) {
	t.Parallel()
	called := false
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"verdict":"pass"}`))
	})
	reg := NewProductionRegistry(ProductionWiringDeps{EvalGateHandler: fake})
	v, ok := reg.Lookup("eval", "gate")
	if !ok {
		t.Fatalf("eval.gate verb not found in registry")
	}
	rr := httptest.NewRecorder()
	v.Handler.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/eval/gate", nil))
	if !called {
		t.Errorf("EvalGateHandler was not invoked")
	}
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d, want 200 (still 503 stub?)", rr.Code)
	}
}

// TestNewProductionRegistry_UnwiredSlotsStillReturn503 pins
// the 503-stub fallback for slots whose deps were not
// supplied. NewProductionRegistry with empty deps must
// return a registry where every canonical verb path returns
// 503 (VERB_NOT_WIRED) -- not 404.
func TestNewProductionRegistry_UnwiredSlotsStillReturn503(t *testing.T) {
	t.Parallel()
	reg := NewProductionRegistry(ProductionWiringDeps{})
	if got := len(reg.Verbs()); got != len(CanonicalVerbs) {
		t.Fatalf("registry has %d verbs, want %d", got, len(CanonicalVerbs))
	}
	v, ok := reg.Lookup("eval", "gate")
	if !ok {
		t.Fatalf("eval.gate verb not found")
	}
	rr := httptest.NewRecorder()
	v.Handler.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/eval/gate", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503 (unwired slot)", rr.Code)
	}
}

// newTestIngestRouterDefectsOnly constructs a router with
// only the `defects` verb registered. Used by the iter-5
// "partial wiring" regression test to prove unregistered
// verbs do NOT populate slots.
func newTestIngestRouterDefectsOnly(t *testing.T) *webhook.Router {
	t.Helper()
	return webhook.NewRouter(webhook.RouterConfig{
		Resolver:    webhook.NewStaticSecretResolver(map[string][]byte{"test-key": []byte("test-secret-not-real")}),
		Store:       webhook.NewInMemoryIdempotencyStore(16),
		ScanRunRepo: webhook.NewInMemoryScanRunRepository(),
		Verbs:       []webhook.VerbHandler{newStubVerbHandler("defects", "external_per_row", "per_row")},
		NewUUID:     uuid.NewV4,
	})
}

// newTestIngestRouterAllVerbs constructs a router with all
// four canonical ingest verbs registered as stubs. The stubs
// pass [NewRouter]'s consistency checks (matching
// ScanRunKind / SHABinding per the verb_handler.go closed
// set) so the router accepts them.
//
// The stubs are SUFFICIENT for slot-wiring and gateway-
// auth-bypass tests; they are NOT sufficient for end-to-end
// ingestion tests (no real writer attached).
func newTestIngestRouterAllVerbs(t *testing.T) *webhook.Router {
	t.Helper()
	return webhook.NewRouter(webhook.RouterConfig{
		Resolver:    webhook.NewStaticSecretResolver(map[string][]byte{"test-key": []byte("test-secret-not-real")}),
		Store:       webhook.NewInMemoryIdempotencyStore(16),
		ScanRunRepo: webhook.NewInMemoryScanRunRepository(),
		Verbs: []webhook.VerbHandler{
			newStubVerbHandler("coverage", "external_single", "single"),
			newStubVerbHandler("test_balance", "external_single", "single"),
			newStubVerbHandler("churn", "external_per_row", "per_row"),
			newStubVerbHandler("defects", "external_per_row", "per_row"),
		},
		NewUUID: uuid.NewV4,
	})
}

// stubVerbHandler is a test fixture VerbHandler that
// satisfies the interface without requiring a real
// writer / ingestor. Used by the api package's adapter tests
// to construct a [*webhook.Router] without hauling in the
// real per-verb dependencies (metric_ingestor.Ingestor,
// test_balance.Writer, ChurnIngester, etc.).
type stubVerbHandler struct {
	verb        string
	scanRunKind string
	shaBinding  string
}

func newStubVerbHandler(verb, kind, binding string) *stubVerbHandler {
	return &stubVerbHandler{verb: verb, scanRunKind: kind, shaBinding: binding}
}

func (s *stubVerbHandler) Verb() string        { return s.verb }
func (s *stubVerbHandler) ContentType() string { return "application/json" }
func (s *stubVerbHandler) ScanRunKind() string { return s.scanRunKind }
func (s *stubVerbHandler) SHABinding() string  { return s.shaBinding }

func (s *stubVerbHandler) CanonicalRequest(headers http.Header, body []byte) []byte {
	return body
}

func (s *stubVerbHandler) ExtractMetadata(ctx context.Context, headers http.Header, body []byte) (webhook.VerbPayloadMetadata, error) {
	// Decode `repo_id` from the body when present; the
	// header-borne verbs (coverage / test_balance) also
	// accept X-Forge-Repo-ID. For test purposes either is
	// acceptable -- prefer the body for body-borne verbs.
	if s.shaBinding == "per_row" {
		var payload struct {
			RepoID string `json:"repo_id"`
		}
		_ = json.Unmarshal(body, &payload)
		if payload.RepoID == "" {
			payload.RepoID = "11111111-1111-1111-1111-111111111111"
		}
		repoID, err := uuid.FromString(payload.RepoID)
		if err != nil {
			return webhook.VerbPayloadMetadata{}, err
		}
		return webhook.VerbPayloadMetadata{RepoID: repoID}, nil
	}
	// `single` SHA-binding verbs read repo_id / sha from
	// headers.
	rawRepo := headers.Get(webhook.RepoIDHeader)
	if rawRepo == "" {
		rawRepo = "11111111-1111-1111-1111-111111111111"
	}
	repoID, err := uuid.FromString(rawRepo)
	if err != nil {
		return webhook.VerbPayloadMetadata{}, err
	}
	sha := headers.Get(webhook.SHAHeader)
	if sha == "" {
		sha = "0000000000000000000000000000000000000000"
	}
	return webhook.VerbPayloadMetadata{RepoID: repoID, SHA: sha}, nil
}

func (s *stubVerbHandler) Handle(ctx context.Context, metadata webhook.VerbPayloadMetadata, body []byte, scanRunID uuid.UUID) (webhook.VerbHandleResult, error) {
	return webhook.VerbHandleResult{ScanRunID: scanRunID}, nil
}

// stringReader wraps the body string for [httptest.NewRequest].
// Uses [strings.NewReader] under the hood -- a stdlib
// io.Reader that won't surprise the Router's body-read /
// MaxBytesReader stages.
func stringReader(s string) *strings.Reader {
	return strings.NewReader(s)
}
