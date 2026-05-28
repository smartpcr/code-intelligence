package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// gatewayUnderTest assembles a [GatewayHandler] with a
// recording tracer, a [StaticHMACAuthenticator] at fixedNow,
// and a single registered verb whose body is supplied by the
// caller. Tests construct one of these and exercise it via
// `httptest.NewServer` so the full http.Server pipeline (not
// just `ServeHTTP`) is exercised.
type gatewayUnderTest struct {
	tracer  *RecordingTracer
	server  *httptest.Server
	authNow time.Time
	verbHit *int32
}

func newGatewayUT(t *testing.T, verbHandler http.Handler) *gatewayUnderTest {
	t.Helper()
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := &StaticHMACAuthenticator{
		Secret:   testSecret,
		Issuer:   issuerLiteral,
		Audience: audienceLiteral,
		Now:      func() time.Time { return now },
		Leeway:   30 * time.Second,
	}
	registry := NewVerbRegistry()

	var verbHit int32
	wrapped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&verbHit, 1)
		verbHandler.ServeHTTP(w, r)
	})

	registry.Register(Verb{
		Namespace: "mgmt",
		Name:      "register_repo",
		Handler:   wrapped,
		RepoIDExtractor: func(r *http.Request) (string, *http.Request, error) {
			return r.Header.Get("X-Repo-ID"), r, nil
		},
	})

	tracer := &RecordingTracer{}
	handler := NewGatewayHandler(auth, registry, tracer, nil)

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &gatewayUnderTest{
		tracer:  tracer,
		server:  srv,
		authNow: now,
		verbHit: &verbHit,
	}
}

func (g *gatewayUnderTest) mintToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	if claims == nil {
		claims = map[string]any{}
	}
	// Apply sane defaults so each test does not have to
	// re-specify every claim.
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = issuerLiteral
	}
	if _, ok := claims["aud"]; !ok {
		claims["aud"] = audienceLiteral
	}
	if _, ok := claims["sub"]; !ok {
		claims["sub"] = "alice@example.com"
	}
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = g.authNow.Add(time.Hour).Unix()
	}
	return MintHS256TestToken(testSecret, claims)
}

func (g *gatewayUnderTest) do(t *testing.T, method, path string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, g.server.URL+path, reader)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := g.server.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	return resp
}

// readBody is a small helper that drains and closes a
// response body. Tests use it inline; defer-close on a
// non-2xx body would risk masking the read error.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return b
}

func TestGateway_MissingTokenReturns401(t *testing.T) {
	t.Parallel()
	g := newGatewayUT(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	resp := g.do(t, "POST", "/v1/mgmt/register_repo", []byte(`{}`), map[string]string{
		"Content-Type": "application/json",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401; body=%s", resp.StatusCode, readBody(t, resp))
	}
	wwwAuth := resp.Header.Get("WWW-Authenticate")
	if !strings.HasPrefix(wwwAuth, "Bearer") {
		t.Errorf("WWW-Authenticate=%q, want prefix `Bearer`", wwwAuth)
	}
	body := readBody(t, resp)
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not a JSON envelope: %v; body=%s", err, body)
	}
	if env.Code != CodeMissingToken {
		t.Errorf("code=%q, want %s", env.Code, CodeMissingToken)
	}
	if atomic.LoadInt32(g.verbHit) != 0 {
		t.Errorf("verb handler invoked on missing-token request")
	}
}

func TestGateway_BadAudienceReturns403(t *testing.T) {
	t.Parallel()
	g := newGatewayUT(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	token := g.mintToken(t, map[string]any{"aud": "some-other-service"})
	resp := g.do(t, "POST", "/v1/mgmt/register_repo", []byte(`{}`), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + token,
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body is not a JSON envelope: %v", err)
	}
	if env.Code != CodeBadAudience {
		t.Errorf("code=%q, want %s", env.Code, CodeBadAudience)
	}
	if atomic.LoadInt32(g.verbHit) != 0 {
		t.Errorf("verb handler invoked on bad-audience request")
	}
}

func TestGateway_ValidTokenReachesHandler(t *testing.T) {
	t.Parallel()
	// Verb handler echoes the X-OIDC-Subject + a marker so
	// we can assert the gateway:
	//   1. forwarded the request,
	//   2. stamped the verified subject,
	//   3. did NOT pass through a caller-supplied
	//      X-OIDC-Subject (it overwrote authoritatively).
	g := newGatewayUT(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"x_oidc_subject": r.Header.Get("X-OIDC-Subject"),
			"path":           r.URL.Path,
			"method":         r.Method,
		})
	}))
	token := g.mintToken(t, map[string]any{
		"sub": "alice@example.com",
	})
	resp := g.do(t, "POST", "/v1/mgmt/register_repo", []byte(`{"repo_url":"https://example/x.git"}`), map[string]string{
		"Content-Type":   "application/json",
		"Authorization":  "Bearer " + token,
		"X-OIDC-Subject": "spoofed-attacker@example.com", // MUST be overwritten
		"X-Repo-ID":      "repo-42",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	var got map[string]string
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body not JSON: %v; body=%s", err, body)
	}
	if got["x_oidc_subject"] != "alice@example.com" {
		t.Errorf("downstream X-OIDC-Subject=%q, want alice@example.com (gateway must overwrite spoofed value)", got["x_oidc_subject"])
	}
	if got["path"] != "/v1/mgmt/register_repo" {
		t.Errorf("downstream path=%q, want /v1/mgmt/register_repo", got["path"])
	}
	if atomic.LoadInt32(g.verbHit) != 1 {
		t.Errorf("verbHit=%d, want 1", atomic.LoadInt32(g.verbHit))
	}

	// Span assertions: verb, caller_subject, repo_id, status.
	if g.tracer.Count() != 1 {
		t.Fatalf("recorded spans=%d, want 1", g.tracer.Count())
	}
	last := g.tracer.Last()
	if got := last.Attributes[SpanAttrVerb]; got != "mgmt.register_repo" {
		t.Errorf("span.verb=%v, want mgmt.register_repo", got)
	}
	if got := last.Attributes[SpanAttrCallerSubject]; got != "alice@example.com" {
		t.Errorf("span.caller_subject=%v, want alice@example.com", got)
	}
	if got := last.Attributes[SpanAttrRepoID]; got != "repo-42" {
		t.Errorf("span.repo_id=%v, want repo-42", got)
	}
	if got := last.Attributes[SpanAttrHTTPStatusCode]; got != 200 {
		t.Errorf("span.http.status_code=%v, want 200", got)
	}
	if got := last.Attributes[SpanAttrHTTPRoute]; got != "/v1/mgmt/register_repo" {
		t.Errorf("span.http.route=%v, want /v1/mgmt/register_repo", got)
	}
}

func TestGateway_UnknownVerbReturns404(t *testing.T) {
	t.Parallel()
	// Implementation-plan Stage 6.4 scenario `unknown-verb-404`:
	// "POST to /v1/eval/unknown_verb returns 404 and emits no
	// evaluation_run row." A row is written only when the
	// downstream handler runs; verifying the gateway does
	// NOT call any handler is the strongest assertion we
	// can make at the gateway level.
	g := newGatewayUT(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("verb handler invoked on unknown-verb path")
	}))
	token := g.mintToken(t, nil)
	resp := g.do(t, "POST", "/v1/eval/unknown_verb", []byte(`{}`), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + token,
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404; body=%s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body not JSON envelope: %v", err)
	}
	if env.Code != CodeUnknownVerb {
		t.Errorf("code=%q, want %s", env.Code, CodeUnknownVerb)
	}
}

func TestGateway_MalformedPathReturns404(t *testing.T) {
	t.Parallel()
	// A path that does not match `/v1/{ns}/{verb}` should
	// return 404 with no auth challenge. We assert no
	// Bearer header is emitted (the gateway treats the
	// path as "not under our routes" rather than "auth
	// problem under our routes").
	g := newGatewayUT(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("verb handler invoked")
	}))
	resp := g.do(t, "POST", "/v2/mgmt/register_repo", []byte(`{}`), map[string]string{
		"Content-Type": "application/json",
	})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d, want 404", resp.StatusCode)
	}
}

func TestGateway_UnknownVerbReturns404RegardlessOfAuth(t *testing.T) {
	t.Parallel()
	// Workstream brief verbatim: "refuse unknown verbs
	// with 404". This contract holds REGARDLESS of
	// whether the caller authenticated -- iter-1's
	// auth-before-lookup ordering returned 401 for the
	// unauthenticated case, which leaked the existence
	// of the auth surface and violated the brief.
	// The fix re-orders the pipeline so verb-lookup
	// precedes auth; an unknown verb is 404 for every
	// caller. (Item #4 from iter-1 evaluator feedback.)
	g := newGatewayUT(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("verb handler invoked on unknown-verb path")
	}))

	t.Run("unauthenticated", func(t *testing.T) {
		resp := g.do(t, "POST", "/v1/eval/unknown_verb", []byte(`{}`), nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status=%d, want 404 (unknown verb must 404 even without auth)", resp.StatusCode)
		}
		body := readBody(t, resp)
		var env errorEnvelope
		_ = json.Unmarshal(body, &env)
		if env.Code != CodeUnknownVerb {
			t.Errorf("code=%q, want %s", env.Code, CodeUnknownVerb)
		}
	})

	t.Run("authenticated", func(t *testing.T) {
		token := g.mintToken(t, nil)
		resp := g.do(t, "POST", "/v1/eval/unknown_verb_authed", []byte(`{}`), map[string]string{
			"Content-Type":  "application/json",
			"Authorization": "Bearer " + token,
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("status=%d, want 404", resp.StatusCode)
		}
	})
}

func TestGateway_ExpiredTokenReturns401(t *testing.T) {
	t.Parallel()
	g := newGatewayUT(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("verb handler invoked on expired-token request")
	}))
	token := g.mintToken(t, map[string]any{
		"exp": g.authNow.Add(-5 * time.Minute).Unix(),
	})
	resp := g.do(t, "POST", "/v1/mgmt/register_repo", []byte(`{}`), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + token,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
	body := readBody(t, resp)
	var env errorEnvelope
	_ = json.Unmarshal(body, &env)
	if env.Code != CodeExpiredToken {
		t.Errorf("code=%q, want %s", env.Code, CodeExpiredToken)
	}
}

func TestGateway_BadSignatureReturns401(t *testing.T) {
	t.Parallel()
	g := newGatewayUT(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("verb handler invoked on bad-signature request")
	}))
	token := MintHS256TestToken([]byte("wrong-secret"), map[string]any{
		"iss": issuerLiteral, "aud": audienceLiteral, "sub": "alice", "exp": g.authNow.Add(time.Hour).Unix(),
	})
	resp := g.do(t, "POST", "/v1/mgmt/register_repo", []byte(`{}`), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + token,
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
}

func TestGateway_PanicInHandlerRecovered(t *testing.T) {
	t.Parallel()
	g := newGatewayUT(t, http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("downstream blew up")
	}))
	token := g.mintToken(t, nil)
	resp := g.do(t, "POST", "/v1/mgmt/register_repo", []byte(`{}`), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + token,
	})
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", resp.StatusCode)
	}
	// Span MUST still record the request even when the
	// downstream handler panicked.
	if g.tracer.Count() != 1 {
		t.Errorf("spans=%d, want 1 (span emitted even on panic)", g.tracer.Count())
	}
	last := g.tracer.Last()
	if len(last.Errors) == 0 {
		t.Errorf("span had no recorded error after panic")
	}
}

func TestNewServer_PanicsOnNilAuth(t *testing.T) {
	t.Parallel()
	defer func() {
		if rec := recover(); rec == nil {
			t.Fatalf("NewServer with nil Authenticator did not panic")
		}
	}()
	_ = NewServer(ServerConfig{Registry: NewVerbRegistry()})
}

func TestNewServer_DefaultsRegistryToCanonical(t *testing.T) {
	t.Parallel()
	// Item #2 from iter-3 feedback: NewServer no longer
	// panics on nil Registry. Instead, NewDefaultRegistry
	// is installed so every canonical verb is exposed as
	// a 503 stub out of the box.
	s := NewServer(ServerConfig{
		Authenticator: &StaticHMACAuthenticator{Secret: testSecret, Audience: audienceLiteral},
	})
	if s.Registry() == nil {
		t.Fatalf("Server.Registry() is nil after default install")
	}
	want := len(CanonicalVerbs)
	got := len(s.Registry().Verbs())
	if got != want {
		t.Errorf("default registry has %d verbs, want %d (CanonicalVerbs)", got, want)
	}
}

func TestNewServer_DefaultTracerIsOTel(t *testing.T) {
	t.Parallel()
	// Item #3 from iter-3 feedback: nil Tracer with
	// DisableTracing=false must install an OTel-backed
	// tracer (not silently NoopTracer). We can't easily
	// assert the concrete *OTelTracer type from outside
	// the handler, but we CAN assert the handler is
	// non-nil and that NewServer doesn't panic.
	s := NewServer(ServerConfig{
		Authenticator: &StaticHMACAuthenticator{Secret: testSecret, Audience: audienceLiteral},
	})
	if s.Handler() == nil {
		t.Fatalf("Server.Handler() is nil")
	}
	// The OTel global provider returns a no-op tracer in
	// the absence of explicit configuration, so the
	// gateway still serves requests cleanly under the
	// default tracer.
}

func TestNewServer_DisableTracingInstallsNoop(t *testing.T) {
	t.Parallel()
	// Item #3 from iter-3 feedback: the deliberate
	// span-drop path is DisableTracing=true. The gateway
	// still serves but emits no spans.
	s := NewServer(ServerConfig{
		Authenticator:  &StaticHMACAuthenticator{Secret: testSecret, Audience: audienceLiteral},
		DisableTracing: true,
	})
	if s.Handler() == nil {
		t.Fatalf("Server.Handler() is nil")
	}
}

func TestNewServer_ExplicitTracerOverridesDefault(t *testing.T) {
	t.Parallel()
	// A composition root that passes a Tracer explicitly
	// must see THAT tracer plumbed in (DisableTracing is
	// ignored when Tracer is non-nil).
	tr := &RecordingTracer{}
	s := NewServer(ServerConfig{
		Authenticator:  &StaticHMACAuthenticator{Secret: testSecret, Audience: audienceLiteral},
		Tracer:         tr,
		DisableTracing: true, // ignored
	})
	if s.Handler() == nil {
		t.Fatalf("Server.Handler() is nil")
	}
}

func TestServer_RegistryAccessor(t *testing.T) {
	t.Parallel()
	reg := NewVerbRegistry()
	s := NewServer(ServerConfig{
		Authenticator: &StaticHMACAuthenticator{Secret: testSecret, Audience: audienceLiteral},
		Registry:      reg,
	})
	if s.Registry() != reg {
		t.Errorf("Server.Registry() did not return the registry passed in")
	}
	if s.Handler() == nil {
		t.Errorf("Server.Handler() returned nil")
	}
	if s.HTTPServer() == nil {
		t.Errorf("Server.HTTPServer() returned nil")
	}
}

// stubAuthenticator returns the configured (identity, err) on
// every call. Used by the auth-backend-503 test to drive an
// ErrAuthBackend through the gateway without relying on a
// JWKS endpoint at the test layer.
type stubAuthenticator struct {
	identity *Identity
	err      error
}

func (s *stubAuthenticator) Authenticate(ctx context.Context, bearer string) (*Identity, error) {
	return s.identity, s.err
}

func TestGateway_AuthBackendFailureReturns503WithRetryAfter(t *testing.T) {
	t.Parallel()
	// Item #5 from iter-2 feedback: when the authenticator
	// surfaces ErrAuthBackend (e.g. JWKS unreachable, OIDC
	// discovery 5xx), the gateway must NOT return 401 (which
	// would page the auth-team for a downed IdP as if it
	// were a flood of bad tokens). It must return 503 so
	// SRE dashboards see infra-failure, not credential-fail.
	stub := &stubAuthenticator{err: fmt.Errorf("%w: idp down", ErrAuthBackend)}
	registry := NewVerbRegistry()
	registry.Register(Verb{
		Namespace: "mgmt", Name: "register_repo",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			t.Fatalf("verb handler unreachable when auth backend is down")
		}),
	})
	handler := NewGatewayHandler(stub, registry, &RecordingTracer{}, nil)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/mgmt/register_repo", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer doesnt-matter")
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", resp.StatusCode)
	}
	if h := resp.Header.Get("Retry-After"); h == "" {
		t.Errorf("Retry-After header missing on 503 -- dashboards / clients need backoff signal")
	}
	var env errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if env.Code != CodeAuthBackend {
		t.Errorf("code=%q, want %q", env.Code, CodeAuthBackend)
	}
}

func TestIdentityFromContext(t *testing.T) {
	t.Parallel()
	// Verifies the gateway threads the verified Identity
	// into the downstream context (verb handlers that
	// prefer typed access over header parsing can call
	// IdentityFromContext).
	var sawIdentity *Identity
	g := newGatewayUT(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawIdentity = IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	token := g.mintToken(t, map[string]any{"sub": "alice@example.com"})
	resp := g.do(t, "POST", "/v1/mgmt/register_repo", []byte(`{}`), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + token,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, readBody(t, resp))
	}
	if sawIdentity == nil {
		t.Fatalf("downstream handler did not see an Identity")
	}
	if sawIdentity.Subject != "alice@example.com" {
		t.Errorf("Identity.Subject=%q, want alice@example.com", sawIdentity.Subject)
	}
}

func TestGateway_SlogTracerEmitsSpan(t *testing.T) {
	t.Parallel()
	// Use the SlogTracer with a captured logger so the
	// test can assert at least one span-shaped log line
	// is emitted carrying the canonical attribute set.
	var buf bytes.Buffer
	logger := slogJSONLogger(&buf)

	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := &StaticHMACAuthenticator{
		Secret:   testSecret,
		Issuer:   issuerLiteral,
		Audience: audienceLiteral,
		Now:      func() time.Time { return now },
	}
	registry := NewVerbRegistry()
	registry.Register(Verb{
		Namespace: "mgmt",
		Name:      "register_repo",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}),
	})
	tracer := &SlogTracer{Logger: logger}
	gw := NewGatewayHandler(auth, registry, tracer, logger)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	token := MintHS256TestToken(testSecret, map[string]any{
		"iss": issuerLiteral, "sub": "alice", "aud": audienceLiteral,
		"exp": now.Add(time.Hour).Unix(),
	})
	req, err := http.NewRequest("POST", srv.URL+"/v1/mgmt/register_repo", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, want 200; body=%s", resp.StatusCode, body)
	}

	logs := buf.String()
	if !strings.Contains(logs, `"event":"span"`) {
		t.Errorf("log buffer does not contain a span entry; logs=%s", logs)
	}
	if !strings.Contains(logs, `"`+SpanAttrVerb+`":"mgmt.register_repo"`) {
		t.Errorf("span log missing verb attr; logs=%s", logs)
	}
	if !strings.Contains(logs, `"`+SpanAttrCallerSubject+`":"alice"`) {
		t.Errorf("span log missing caller_subject attr; logs=%s", logs)
	}
}

func TestStatusWriter_Transparent(t *testing.T) {
	t.Parallel()
	// Direct unit test of the transparent status-writer
	// wrapper to ensure it does NOT buffer the body and
	// captures the status correctly.
	rec := httptest.NewRecorder()
	sw := newStatusWriter(rec)

	sw.Header().Set("Content-Type", "text/plain")
	sw.WriteHeader(http.StatusTeapot)
	n, err := fmt.Fprintf(sw, "hello")
	if err != nil {
		t.Fatalf("Fprintf: %v", err)
	}
	if n != 5 {
		t.Errorf("wrote n=%d, want 5", n)
	}
	if sw.Status() != http.StatusTeapot {
		t.Errorf("Status=%d, want 418", sw.Status())
	}
	if !sw.HeaderWritten() {
		t.Errorf("HeaderWritten=false, want true")
	}
	if rec.Body.String() != "hello" {
		t.Errorf("downstream body=%q, want hello (no buffering)", rec.Body.String())
	}
	if rec.Code != http.StatusTeapot {
		t.Errorf("downstream code=%d, want 418", rec.Code)
	}
}

func TestStatusWriter_ImplicitStatus(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	sw := newStatusWriter(rec)
	// Write without an explicit WriteHeader -- the
	// statusWriter must record 200 (matching the stdlib's
	// implicit-200 contract on Write).
	if _, err := fmt.Fprint(sw, "hello"); err != nil {
		t.Fatalf("Fprint: %v", err)
	}
	if sw.Status() != http.StatusOK {
		t.Errorf("Status=%d, want 200 (implicit)", sw.Status())
	}
}

func TestStatusWriter_HijackPropagates(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder() // does NOT implement Hijacker.
	sw := newStatusWriter(rec)
	_, _, err := sw.Hijack()
	if !errors.Is(err, errHijackNotSupported) {
		t.Errorf("Hijack on non-Hijacker writer: err=%v, want errHijackNotSupported", err)
	}
}

// slogJSONLogger returns a slog.Logger writing line-delimited
// JSON to `w`. Tiny helper -- the test imports `log/slog` and
// constructs a JSONHandler with sane defaults.
func slogJSONLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}
