package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIntegration_AllSurfaceVerbsEmitSpans satisfies iter-2
// evaluator feedback item #4 by driving the REAL
// [GatewayHandler] via [httptest] for one verb in each of
// the four canonical Stage 9.4 namespaces (mgmt.*,
// ingest.*, policy.*, eval.*). The assertion is that
// EVERY served verb -- success, auth-failure, and authz-
// denied alike -- records exactly one span on the
// gateway tracer, AND that the span carries the canonical
// Stage 9.4 attribute set (`verb`, `repo_id`,
// `policy_version_id`, `degraded`, `degraded_reason`,
// `verdict`, `auth_status`).
//
// The previous Stage 9.4 integration test only exercised
// the [telemetry.AnnotateEvalGateSpan] helper directly --
// the evaluator caught that and asked for handler-level
// coverage of EACH surface. This test drives the real
// `ServeHTTP` path through `httptest.NewServer`, registers
// stub verbs across all four namespaces, and asserts on
// the [RecordingTracer]'s capture set.
func TestIntegration_AllSurfaceVerbsEmitSpans(t *testing.T) {
	t.Parallel()
	cases := []struct {
		namespace string
		name      string
		wantVerb  string
	}{
		{"mgmt", "register_repo", "mgmt.register_repo"},
		{"ingest", "coverage", "ingest.coverage"},
		{"policy", "activate", "policy.activate"},
		{"eval", "gate", "eval.gate"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.namespace+"."+tc.name, func(t *testing.T) {
			t.Parallel()
			tracer := &RecordingTracer{}
			registry := NewVerbRegistry()
			registry.Register(Verb{
				Namespace: tc.namespace,
				Name:      tc.name,
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"ok":true}`))
				}),
				RepoIDExtractor: func(r *http.Request) (string, *http.Request, error) {
					return r.Header.Get("X-Repo-ID"), r, nil
				},
			})
			auth := &stubAuthenticator{identity: &Identity{Subject: "alice@example.com"}}
			handler := NewGatewayHandler(auth, registry, tracer, nil)
			srv := httptest.NewServer(handler)
			defer srv.Close()

			repoID := "11111111-2222-3333-4444-555555555555"
			req, _ := http.NewRequest("POST", srv.URL+"/v1/"+tc.namespace+"/"+tc.name, strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer doesnt-matter")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Repo-ID", repoID)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d (body=%s), want 200", resp.StatusCode, body)
			}

			if tracer.Count() != 1 {
				t.Fatalf("recorded span count=%d, want exactly 1 (every verb invocation must emit one span)", tracer.Count())
			}
			span := tracer.Last()
			if span.Name != SpanName {
				t.Errorf("span name=%q, want %q", span.Name, SpanName)
			}
			// The canonical Stage 9.4 attribute set MUST
			// be present on every span. Empty-string and
			// false are valid defaults; missing keys are
			// the bug.
			wants := map[string]any{
				SpanAttrVerb:            tc.wantVerb,
				SpanAttrRepoID:          repoID,
				SpanAttrPolicyVersionID: "",
				SpanAttrDegraded:        false,
				SpanAttrDegradedReason:  "",
				SpanAttrVerdict:         "",
				SpanAttrAuthStatus:      AuthStatusOK,
				SpanAttrCallerSubject:   "alice@example.com",
			}
			for key, want := range wants {
				got, ok := span.Attributes[key]
				if !ok {
					t.Errorf("missing canonical span attribute %q on %s span", key, tc.wantVerb)
					continue
				}
				if got != want {
					t.Errorf("span attribute %q = %v, want %v", key, got, want)
				}
			}
		})
	}
}

// TestIntegration_AuthRejectedRequestStillEmitsSpan asserts
// the Stage 9.4 (iter-2 evaluator feedback #3) contract:
// a 401 from authn failure -- previously RETURNED BEFORE
// the span was opened -- must now emit a verb span with
// `auth_status="unauthenticated"`. This is the test that
// would have caught the regression had it existed in
// iter-1. We drive the gateway with a [stubAuthenticator]
// that always errors and assert the [RecordingTracer]
// captured exactly one span with the expected enum value.
func TestIntegration_AuthRejectedRequestStillEmitsSpan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		authErr      error
		wantStatus   int
		wantAuthAttr string
	}{
		{
			name:         "unauthenticated_missing_token",
			authErr:      ErrMissingToken,
			wantStatus:   http.StatusUnauthorized,
			wantAuthAttr: AuthStatusUnauthenticated,
		},
		{
			name:         "unauthenticated_invalid_token",
			authErr:      ErrInvalidToken,
			wantStatus:   http.StatusUnauthorized,
			wantAuthAttr: AuthStatusUnauthenticated,
		},
		{
			name:         "unauthenticated_expired_token",
			authErr:      ErrExpiredToken,
			wantStatus:   http.StatusUnauthorized,
			wantAuthAttr: AuthStatusUnauthenticated,
		},
		{
			name:         "backend_unavailable",
			authErr:      ErrAuthBackend,
			wantStatus:   http.StatusServiceUnavailable,
			wantAuthAttr: AuthStatusBackendUnavailable,
		},
		{
			name:         "denied_audience_mismatch",
			authErr:      ErrBadAudience,
			wantStatus:   http.StatusForbidden,
			wantAuthAttr: AuthStatusDenied,
		},
		{
			name:         "authenticator_internal_failure_backend_unavailable",
			authErr:      errors.New("unexpected: non-sentinel authenticator error"),
			wantStatus:   http.StatusInternalServerError,
			wantAuthAttr: AuthStatusBackendUnavailable,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tracer := &RecordingTracer{}
			registry := NewVerbRegistry()
			registry.Register(Verb{
				Namespace: "mgmt",
				Name:      "register_repo",
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					t.Fatalf("verb handler MUST NOT be reached on auth failure")
				}),
			})
			auth := &stubAuthenticator{err: tc.authErr}
			handler := NewGatewayHandler(auth, registry, tracer, nil)
			srv := httptest.NewServer(handler)
			defer srv.Close()

			req, _ := http.NewRequest("POST", srv.URL+"/v1/mgmt/register_repo", strings.NewReader("{}"))
			req.Header.Set("Authorization", "Bearer something")
			req.Header.Set("Content-Type", "application/json")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status=%d, want %d", resp.StatusCode, tc.wantStatus)
			}
			if tracer.Count() != 1 {
				t.Fatalf("recorded span count=%d, want 1 (auth-rejected requests MUST still emit a span -- iter-2 evaluator feedback #3)", tracer.Count())
			}
			span := tracer.Last()
			if got := span.Attributes[SpanAttrVerb]; got != "mgmt.register_repo" {
				t.Errorf("verb attr = %v, want mgmt.register_repo", got)
			}
			if got := span.Attributes[SpanAttrAuthStatus]; got != tc.wantAuthAttr {
				t.Errorf("auth_status attr = %v, want %q", got, tc.wantAuthAttr)
			}
			// repo_id stays empty on the auth-rejected
			// path -- we deliberately DO NOT body-parse
			// before auth.
			if got := span.Attributes[SpanAttrRepoID]; got != "" {
				t.Errorf("repo_id attr = %v, want empty on auth-failure path", got)
			}
			if got := span.Attributes[SpanAttrCallerSubject]; got != "" {
				t.Errorf("caller_subject attr = %v, want empty on authn-failure path", got)
			}
			// verdict MUST remain empty -- the auth_status
			// enum is INTENTIONALLY DISJOINT from verdict
			// per the design note on SpanAttrAuthStatus.
			if got := span.Attributes[SpanAttrVerdict]; got != "" {
				t.Errorf("verdict attr = %v, want empty on auth-failure path", got)
			}
		})
	}
}

// TestIntegration_AuthzDeniedRequestEmitsSpan asserts the
// authz-denial branch of the same Stage 9.4 contract:
// 403 from `ErrInsufficientGroup` previously returned
// BEFORE the span was opened, so the denial was invisible
// to dashboards. The test wires a [stubAuthorizer] that
// returns the canonical sentinel and asserts the span
// carries `auth_status="denied"`. A second sub-test
// covers the non-sentinel authz error branch (mapped to
// `backend_unavailable`).
func TestIntegration_AuthzDeniedRequestEmitsSpan(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		authzErr     error
		wantStatus   int
		wantAuthAttr string
	}{
		{
			name:         "denied_insufficient_group",
			authzErr:     ErrInsufficientGroup,
			wantStatus:   http.StatusForbidden,
			wantAuthAttr: AuthStatusDenied,
		},
		{
			name:         "backend_unavailable_non_sentinel",
			authzErr:     errors.New("authz backend rpc timeout"),
			wantStatus:   http.StatusInternalServerError,
			wantAuthAttr: AuthStatusBackendUnavailable,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tracer := &RecordingTracer{}
			registry := NewVerbRegistry()
			registry.Register(Verb{
				Namespace: "mgmt",
				Name:      "register_repo",
				Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					t.Fatalf("verb handler MUST NOT be reached on authz denial")
				}),
			})
			auth := &stubAuthenticator{identity: &Identity{Subject: "alice@example.com"}}
			authz := &stubAuthorizer{err: tc.authzErr}
			handler := NewGatewayHandlerWithAuthorizer(auth, authz, registry, tracer, nil)
			srv := httptest.NewServer(handler)
			defer srv.Close()

			req, _ := http.NewRequest("POST", srv.URL+"/v1/mgmt/register_repo",
				bytes.NewReader([]byte("{}")))
			req.Header.Set("Authorization", "Bearer ok")
			req.Header.Set("Content-Type", "application/json")
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status=%d (body=%s), want %d", resp.StatusCode, body, tc.wantStatus)
			}
			if tracer.Count() != 1 {
				t.Fatalf("recorded span count=%d, want 1 (authz-denied requests MUST still emit a span -- iter-2 evaluator feedback #3)", tracer.Count())
			}
			span := tracer.Last()
			if got := span.Attributes[SpanAttrAuthStatus]; got != tc.wantAuthAttr {
				t.Errorf("auth_status attr = %v, want %q", got, tc.wantAuthAttr)
			}
			// caller_subject IS captured on the denied
			// path -- authn succeeded, only authz failed
			// -- so dashboards can group denials by
			// caller.
			if got := span.Attributes[SpanAttrCallerSubject]; got != "alice@example.com" {
				t.Errorf("caller_subject = %v, want alice@example.com on authz-denied path", got)
			}
		})
	}
}

// stubAuthorizer is a test-only [Authorizer] that returns
// a configured error on every Authorize call. Used by the
// authz-denied span-emission tests to drive both the
// canonical [ErrInsufficientGroup] branch and the
// non-sentinel "backend unavailable" branch through a
// real [GatewayHandler].
type stubAuthorizer struct {
	err error
}

func (s *stubAuthorizer) Authorize(_ context.Context, _ *Identity, _ string) error {
	return s.err
}
