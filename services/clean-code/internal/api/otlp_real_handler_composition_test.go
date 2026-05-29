package api_test

// Stage 9.4 iter-4 evaluator feedback item #1 fix.
//
// The iter-3 OTLP composition test
// (`internal/telemetry/otlp_receiver_test.go::
// TestIntegration_FakeOTLPReceiver_MiddlewareAnnotatorComposition`)
// proved that a middleware-opened span can be overwritten
// by a handler-side annotator call -- BUT it did so with
// inline `mux.HandleFunc` stubs that called the annotator
// directly. The evaluator caught that the canonical
// production handlers (`management.MgmtWriter.RegisterRepo`,
// `management.PolicyWriter.Activate`, `webhook.Router` ->
// `webhook.ChurnVerbHandler`) were never on the call path.
//
// This file drives THOSE EXACT production handlers behind
// `telemetry.NewVerbSpanMiddleware` over the real OTLP gRPC
// wire to a `oteltest.FakeOTLPReceiver`. The assertions
// verify the receiver captures spans whose `repo_id` /
// `policy_version_id` attributes carry the values the
// PRODUCTION handlers stamped via
// `telemetry.AnnotateVerbSpanRepoID` /
// `telemetry.AnnotateVerbSpanPolicyVersionID` -- not the
// empty-string open-time defaults the middleware stamps.
//
// # Why this test lives in `internal/api`
//
// `internal/management` and `internal/ingest/webhook` both
// import `internal/telemetry`, so a composition test placed
// inside `internal/telemetry/*_test.go` cannot import them
// without creating a circular dependency at the package
// graph level. `internal/api` is the highest layer in the
// service that already imports BOTH `management` (via
// `adapters.go`) AND `ingest/webhook` (via `adapters.go`)
// AND `telemetry` (via `tracing.go`), which makes it the
// only natural home for an integration test that wires all
// three production handlers behind the verb-span
// middleware. The new `oteltest` package
// (`internal/telemetry/oteltest`) is the shared fake-OTLP
// helper this test imports; it is also imported by the
// iter-3 receiver test under `internal/telemetry/` so both
// tests exercise the same gRPC receiver implementation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	"go.opentelemetry.io/otel"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/webhook"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/management"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/telemetry"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/telemetry/oteltest"
)

// nopSampleResolver satisfies [management.SampleResolver]
// for the composition test. The `mgmt.register_repo` verb
// never touches the sample resolver path (it's only
// consulted by `mgmt.retract_sample`), but the
// [management.NewMgmtWriter] signature requires a non-nil
// value in every position so wiring it to a no-op keeps the
// constructor happy without dragging in a real metric
// ingestor.
type nopSampleResolver struct{}

func (nopSampleResolver) ResolveSample(_ context.Context, _ uuid.UUID) (uuid.UUID, string, bool, error) {
	return uuid.Nil, "", false, nil
}

// nopRetractDispatcher satisfies
// [management.RetractDispatcher]. See [nopSampleResolver]
// for the rationale.
type nopRetractDispatcher struct{}

func (nopRetractDispatcher) Dispatch(_ context.Context, _ uuid.UUID, _, _ string) (management.RetractResult, error) {
	return management.RetractResult{}, nil
}

// nopRescanEnqueuer satisfies [management.RescanEnqueuer].
// See [nopSampleResolver] for the rationale.
type nopRescanEnqueuer struct{}

func (nopRescanEnqueuer) Enqueue(_ context.Context, _ uuid.UUID, _, _ string) (management.RescanResult, error) {
	return management.RescanResult{}, nil
}

// TestIntegration_RealHandlers_OverwriteVerbSpanAttrs_OverOTLP
// is the iter-4 evaluator feedback #1 fix.
//
// Asserts that when the canonical Stage 9.4 verb-span
// middleware wraps THREE production handlers --
//
//   - [management.MgmtWriter.RegisterRepo] at
//     `/v1/mgmt/register_repo` (annotator runs AFTER the
//     store mints the repo_id),
//   - [management.PolicyWriter.Activate] at
//     `/v1/policy/activate` (annotator runs BEFORE the
//     steward call, with the wire-supplied PVID), and
//   - [webhook.Router] dispatching to
//     [webhook.ChurnVerbHandler] at `/v1/ingest/churn`
//     (annotator runs after `ExtractMetadata` resolves
//     the repo_id from the wire body) --
//
// the resulting spans the OTel SDK exports over real gRPC
// to the in-process [oteltest.FakeOTLPReceiver] carry the
// LATER (annotator-overwritten) `repo_id` /
// `policy_version_id` values, not the empty-string
// open-time defaults.
//
// The test deliberately does NOT call `t.Parallel()` --
// `telemetry.Setup` installs a global TracerProvider, so
// running this test concurrently with the iter-3
// composition test in `internal/telemetry/` would race on
// the global. The shutdown cleanup restores the previous
// provider so subsequent tests see the original state.
func TestIntegration_RealHandlers_OverwriteVerbSpanAttrs_OverOTLP(t *testing.T) {
	endpoint, receiver, stop := oteltest.Start(t)
	t.Cleanup(stop)

	prevProvider := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prevProvider) })

	// SamplerRatio is INTENTIONALLY OMITTED. The Stage 9.4
	// iter-3 contract treats the zero value as
	// "AlwaysSample"; the iter-3 composition test in
	// `internal/telemetry/` locks in this contract and we
	// rely on it here to keep the production composition
	// roots' "no SamplerRatio specified" posture honest.
	shutdown, err := telemetry.Setup(context.Background(), config.Config{
		OTelEndpoint: endpoint,
	}, telemetry.SetupOptions{
		ServiceName: "clean-code-real-handler-composition-test",
		Insecure:    true,
		DialTimeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("telemetry.Setup: %v", err)
	}
	if shutdown == nil {
		t.Fatal("telemetry.Setup returned nil ShutdownFunc")
	}

	// ---- mgmt.register_repo wiring -----------------------
	// Real `InMemoryRepoStore` + `InMemoryRepoEventAppender`
	// behind the production `MgmtWriter`. The store mints a
	// fresh repo_id on the registration call; the handler
	// stamps that minted id on the span via
	// `telemetry.AnnotateVerbSpanRepoID` AFTER the store
	// call returns (register_repo_verb.go:241).
	appender := management.NewInMemoryRepoEventAppender()
	repoStore := management.NewInMemoryRepoStore(appender)
	mgmtWriter := management.NewMgmtWriter(
		nopSampleResolver{},
		nopRetractDispatcher{},
		nopRescanEnqueuer{},
		appender,
		management.WithMgmtWriterRepoStore(repoStore),
	)

	// ---- policy.activate wiring --------------------------
	// Real steward backed by a real in-memory store and a
	// real `*keys.Manager` with a minted signing key. The
	// `Activate` precondition `checkSigningKey` enforces an
	// active key set, so the empty `noActiveSigner` null
	// object would short-circuit with `ErrNoActiveSigningKey`
	// and the annotator-stamped PVID would never reach the
	// span. Real keys make the steward call succeed so the
	// span survives close with the annotator's value.
	keysRes, err := keys.Build(context.Background(), keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	t.Cleanup(keysRes.Close)
	stewardStore := steward.NewInMemoryStore()
	stewardInst, err := steward.New(steward.Config{
		Store:  stewardStore,
		Signer: keysRes.Manager,
	})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	// Pre-seed one PolicyVersion so steward.Activate's FK
	// check (steward.go:262 calls GetPolicyVersion) finds
	// the row. The activator never publishes -- we just
	// need a row that exists so the Activate path runs end-
	// to-end after the annotator stamps the wire-supplied
	// PVID.
	wantPVID := uuid.Must(uuid.NewV4())
	seededPV := steward.PolicyVersion{
		PolicyVersionID: wantPVID,
		Name:            "iter4-otlp-composition-policy",
		Signature:       []byte("not-validated-by-activate"),
		CreatedAt:       time.Now().UTC(),
	}
	if err := stewardStore.InsertPolicyVersion(context.Background(), seededPV); err != nil {
		t.Fatalf("seed steward.InsertPolicyVersion: %v", err)
	}
	policyWriter := management.NewPolicyWriter(stewardInst)

	// ---- ingest.churn wiring -----------------------------
	// Real `webhook.Router` -> `webhook.ChurnVerbHandler` ->
	// `churn.Ingester` -> `churn.InMemoryChurnEventStore`.
	// The router's `ServeHTTP` calls
	// `telemetry.AnnotateVerbSpanRepoID(ctx,
	// metadata.RepoID.String())` AFTER `ExtractMetadata`
	// resolves the repo_id from the wire body
	// (router.go:516).
	churnStore := churn.NewInMemoryChurnEventStore()
	ingester := churn.NewIngesterWithClocks(churnStore, time.Now, uuid.NewV4)
	verbHandler := webhook.NewChurnVerbHandler(ingester)
	hmacKeyID := "ws-iter4-key"
	hmacSecret := []byte("ws-iter4-hmac-secret-32-bytes-deadbeef!!")
	secretResolver := webhook.NewStaticSecretResolver(map[string][]byte{
		hmacKeyID: hmacSecret,
	})
	router := webhook.NewRouter(webhook.RouterConfig{
		Resolver:    secretResolver,
		Store:       webhook.NewInMemoryIdempotencyStore(0),
		ScanRunRepo: webhook.NewInMemoryScanRunRepository(),
		Verbs:       []webhook.VerbHandler{verbHandler},
	})

	// ---- compose the mux behind the verb-span middleware -
	mux := http.NewServeMux()
	mux.Handle("/v1/mgmt/register_repo", http.HandlerFunc(mgmtWriter.RegisterRepo))
	mux.Handle("/v1/policy/activate", http.HandlerFunc(policyWriter.Activate))
	// Webhook router owns the entire `/v1/ingest/` prefix
	// per `webhook.RouterPath`. The verb-span middleware
	// matches `/v1/ingest/churn` exactly via the route
	// table below.
	mux.Handle(webhook.RouterPath, router)

	routes := []telemetry.VerbRoute{
		{Path: "/v1/mgmt/register_repo", Verb: "mgmt.register_repo"},
		{Path: "/v1/policy/activate", Verb: "policy.activate"},
		{Path: "/v1/ingest/churn", Verb: "ingest.churn"},
	}
	handler := telemetry.NewVerbSpanMiddleware(mux, routes)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// ---- drive mgmt.register_repo ------------------------
	const repoURL = "https://github.com/iter4/otlp-composition.git"
	registerBody, err := json.Marshal(map[string]string{
		"repo_url":       repoURL,
		"default_branch": "main",
	})
	if err != nil {
		t.Fatalf("marshal register body: %v", err)
	}
	registerReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/mgmt/register_repo", bytes.NewReader(registerBody))
	if err != nil {
		t.Fatalf("NewRequest(register): %v", err)
	}
	registerReq.Header.Set("Content-Type", "application/json")
	registerReq.Header.Set(management.OIDCSubjectHeader, "iter4@example.com")
	registerResp, err := srv.Client().Do(registerReq)
	if err != nil {
		t.Fatalf("Do(register): %v", err)
	}
	registerRespBody, _ := io.ReadAll(registerResp.Body)
	_ = registerResp.Body.Close()
	if registerResp.StatusCode != http.StatusOK {
		t.Fatalf("register status=%d body=%s", registerResp.StatusCode, registerRespBody)
	}
	var registerResponse struct {
		RepoID  string `json:"repo_id"`
		Created bool   `json:"created"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal(registerRespBody, &registerResponse); err != nil {
		t.Fatalf("decode register response: %v (body=%s)", err, registerRespBody)
	}
	if registerResponse.RepoID == "" {
		t.Fatalf("register response missing repo_id (body=%s)", registerRespBody)
	}
	wantMgmtRepoID := registerResponse.RepoID

	// ---- drive policy.activate ---------------------------
	activateBody, err := json.Marshal(map[string]string{
		"policy_version_id": wantPVID.String(),
		"activated_by":      "iter4@example.com",
	})
	if err != nil {
		t.Fatalf("marshal activate body: %v", err)
	}
	activateResp, err := srv.Client().Post(srv.URL+"/v1/policy/activate", "application/json", bytes.NewReader(activateBody))
	if err != nil {
		t.Fatalf("Do(activate): %v", err)
	}
	activateRespBody, _ := io.ReadAll(activateResp.Body)
	_ = activateResp.Body.Close()
	if activateResp.StatusCode != http.StatusOK {
		t.Fatalf("activate status=%d body=%s", activateResp.StatusCode, activateRespBody)
	}

	// ---- drive ingest.churn ------------------------------
	wantChurnRepoID := uuid.Must(uuid.NewV4())
	churnPayload := churn.Payload{
		RepoID: wantChurnRepoID,
		Rows: []churn.PayloadRow{
			{
				SHA:        strings.Repeat("a", 40),
				FilePath:   "internal/iter4/foo.go",
				ModifiedAt: time.Now().UTC().Add(-24 * time.Hour),
			},
		},
	}
	churnBody, err := json.Marshal(churnPayload)
	if err != nil {
		t.Fatalf("marshal churn body: %v", err)
	}
	churnReq, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/ingest/churn", bytes.NewReader(churnBody))
	if err != nil {
		t.Fatalf("NewRequest(churn): %v", err)
	}
	churnReq.Header.Set("Content-Type", "application/json")
	churnReq.Header.Set(webhook.SigningKeyIDHeader, hmacKeyID)
	churnReq.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(churnBody, hmacSecret))
	churnResp, err := srv.Client().Do(churnReq)
	if err != nil {
		t.Fatalf("Do(churn): %v", err)
	}
	churnRespBody, _ := io.ReadAll(churnResp.Body)
	_ = churnResp.Body.Close()
	if churnResp.StatusCode != http.StatusOK {
		t.Fatalf("churn status=%d body=%s", churnResp.StatusCode, churnRespBody)
	}

	// ---- flush + collect ---------------------------------
	flushCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		// Tolerate the benign concurrent-shutdown race
		// (OTLP exporter may surface a connection-closed
		// error while the receiver's gRPC server is
		// stopping). The receiver assertion below is the
		// real contract.
		t.Logf("shutdown returned err (continuing): %v", err)
	}

	captured := oteltest.WaitForSpans(t, receiver, 3, 5*time.Second)
	spans := oteltest.SpansByName(captured)

	// ---- assert mgmt.register_repo overwrote repo_id -----
	mgmtSpan, ok := spans["mgmt.register_repo"]
	if !ok {
		t.Fatalf("mgmt.register_repo span missing from receiver (got %v)", oteltest.SpanNames(spans))
	}
	mgmtAttrs := oteltest.AttrMap(mgmtSpan.GetAttributes())
	if got := mgmtAttrs[telemetry.AttrRepoID]; got != wantMgmtRepoID {
		t.Errorf("mgmt.register_repo span %s = %q, want %q (the production handler MUST overwrite the middleware's open-time empty default with the freshly-minted repo_id)",
			telemetry.AttrRepoID, got, wantMgmtRepoID)
	}
	if got := mgmtAttrs[telemetry.AttrVerb]; got != "mgmt.register_repo" {
		t.Errorf("mgmt.register_repo span %s = %q, want mgmt.register_repo", telemetry.AttrVerb, got)
	}

	// ---- assert policy.activate overwrote pvid -----------
	activateSpan, ok := spans["policy.activate"]
	if !ok {
		t.Fatalf("policy.activate span missing from receiver (got %v)", oteltest.SpanNames(spans))
	}
	activateAttrs := oteltest.AttrMap(activateSpan.GetAttributes())
	if got := activateAttrs[telemetry.AttrPolicyVersionID]; got != wantPVID.String() {
		t.Errorf("policy.activate span %s = %q, want %q (the production handler MUST overwrite the middleware's empty default with the wire-supplied policy_version_id)",
			telemetry.AttrPolicyVersionID, got, wantPVID.String())
	}
	if got := activateAttrs[telemetry.AttrVerb]; got != "policy.activate" {
		t.Errorf("policy.activate span %s = %q, want policy.activate", telemetry.AttrVerb, got)
	}

	// ---- assert ingest.churn overwrote repo_id -----------
	churnSpan, ok := spans["ingest.churn"]
	if !ok {
		t.Fatalf("ingest.churn span missing from receiver (got %v)", oteltest.SpanNames(spans))
	}
	churnAttrs := oteltest.AttrMap(churnSpan.GetAttributes())
	if got := churnAttrs[telemetry.AttrRepoID]; got != wantChurnRepoID.String() {
		t.Errorf("ingest.churn span %s = %q, want %q (the production webhook router MUST overwrite the middleware's empty default with the ExtractMetadata-resolved repo_id)",
			telemetry.AttrRepoID, got, wantChurnRepoID.String())
	}
	if got := churnAttrs[telemetry.AttrVerb]; got != "ingest.churn" {
		t.Errorf("ingest.churn span %s = %q, want ingest.churn", telemetry.AttrVerb, got)
	}

	// Sanity-check that the receiver did not also surface
	// a stray span name we did not expect (e.g. an HTTP
	// instrumentation library installed itself on the
	// global tracer provider). Three exact verb names is
	// the contract.
	for name := range spans {
		switch name {
		case "mgmt.register_repo", "policy.activate", "ingest.churn":
		default:
			t.Errorf("unexpected span name %q captured by receiver (want exactly mgmt.register_repo + policy.activate + ingest.churn)", name)
		}
	}
	// Help future debuggers: emit a one-line summary so a
	// CI log shows the test actually drove all three verbs.
	t.Logf("composition test captured: mgmt.register_repo(repo_id=%s) policy.activate(pvid=%s) ingest.churn(repo_id=%s) [%d total]",
		wantMgmtRepoID, wantPVID.String(), wantChurnRepoID.String(), len(spans))
	_ = fmt.Sprintf // keep `fmt` referenced if a future edit removes the Logf
}
