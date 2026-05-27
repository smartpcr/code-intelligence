package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNoopAuthorizer_AdmitsEveryAuthenticatedCaller asserts
// the default Authorizer accepts every authenticated caller
// regardless of claims. Preserves backward compatibility for
// callers that pass nil for ServerConfig.Authorizer.
func TestNoopAuthorizer_AdmitsEveryAuthenticatedCaller(t *testing.T) {
	var az NoopAuthorizer
	id := &Identity{Subject: "alice@example.com"}
	for _, verb := range []string{"mgmt.read.repo", "policy.publish", "eval.gate"} {
		if err := az.Authorize(context.Background(), id, verb); err != nil {
			t.Errorf("NoopAuthorizer.Authorize(%q): want nil, got %v", verb, err)
		}
	}
}

func TestGroupClaimAuthorizer_NilReceiverErrors(t *testing.T) {
	var az *GroupClaimAuthorizer
	err := az.Authorize(context.Background(), &Identity{Subject: "x"}, "mgmt.read.repo")
	if err == nil {
		t.Fatalf("Authorize on nil receiver: want error, got nil")
	}
}

func TestGroupClaimAuthorizer_RejectsNilIdentity(t *testing.T) {
	az := NewGroupClaimAuthorizer("")
	err := az.Authorize(context.Background(), nil, "mgmt.read.repo")
	if err == nil {
		t.Fatalf("Authorize(nil identity): want error, got nil")
	}
	if errors.Is(err, ErrInsufficientGroup) {
		t.Errorf("Authorize(nil identity): want non-sentinel internal error, got ErrInsufficientGroup")
	}
}

func TestGroupClaimAuthorizer_AbsentPolicyOpens(t *testing.T) {
	az := &GroupClaimAuthorizer{
		ClaimName:       "groups",
		VerbGroupPolicy: map[string][]string{"mgmt.read.repo": {GroupReaders}},
	}
	id := identityWithGroups(t, "someone@example.com", nil)
	err := az.Authorize(context.Background(), id, "verb.with.no.policy")
	if err != nil {
		t.Errorf("verb without policy entry: want nil, got %v", err)
	}
}

func TestGroupClaimAuthorizer_AdmitsMatchingGroup(t *testing.T) {
	az := NewGroupClaimAuthorizer("")
	id := identityWithGroups(t, "alice@example.com", []string{GroupReaders})
	if err := az.Authorize(context.Background(), id, "mgmt.read.repo"); err != nil {
		t.Errorf("Authorize(reader -> mgmt.read.repo): want nil, got %v", err)
	}
}

func TestGroupClaimAuthorizer_AdmitsAdminOnReaderVerb(t *testing.T) {
	az := NewGroupClaimAuthorizer("")
	id := identityWithGroups(t, "carol@example.com", []string{GroupAdmins})
	if err := az.Authorize(context.Background(), id, "mgmt.read.repo"); err != nil {
		t.Errorf("Authorize(admin -> mgmt.read.repo): want nil (admins inherit reader), got %v", err)
	}
}

func TestGroupClaimAuthorizer_RejectsCIOnAdminVerb(t *testing.T) {
	az := NewGroupClaimAuthorizer("")
	id := identityWithGroups(t, "bob@example.com", []string{GroupCI})
	err := az.Authorize(context.Background(), id, "policy.publish")
	if err == nil {
		t.Fatalf("Authorize(ci -> policy.publish): want error, got nil")
	}
	if !errors.Is(err, ErrInsufficientGroup) {
		t.Errorf("Authorize: want ErrInsufficientGroup, got %v", err)
	}
}

func TestGroupClaimAuthorizer_RejectsEmptyGroups(t *testing.T) {
	az := NewGroupClaimAuthorizer("")
	id := identityWithGroups(t, "dave@example.com", []string{})
	err := az.Authorize(context.Background(), id, "mgmt.read.repo")
	if err == nil {
		t.Fatalf("Authorize(no groups -> reader verb): want error, got nil")
	}
	if !errors.Is(err, ErrInsufficientGroup) {
		t.Errorf("Authorize: want ErrInsufficientGroup, got %v", err)
	}
}

func TestGroupClaimAuthorizer_RejectsMissingClaim(t *testing.T) {
	az := NewGroupClaimAuthorizer("")
	id := &Identity{
		Subject:   "eve@example.com",
		RawClaims: map[string]json.RawMessage{"some_other_claim": json.RawMessage(`"value"`)},
	}
	err := az.Authorize(context.Background(), id, "mgmt.read.repo")
	if err == nil {
		t.Fatalf("Authorize(missing claim -> gated verb): want error, got nil")
	}
	if !errors.Is(err, ErrInsufficientGroup) {
		t.Errorf("Authorize: want ErrInsufficientGroup, got %v", err)
	}
}

func TestGroupClaimAuthorizer_AcceptsSingleStringGroupClaim(t *testing.T) {
	az := NewGroupClaimAuthorizer("")
	id := &Identity{
		Subject:   "frank@example.com",
		RawClaims: map[string]json.RawMessage{"groups": json.RawMessage(`"` + GroupAdmins + `"`)},
	}
	if err := az.Authorize(context.Background(), id, "policy.publish"); err != nil {
		t.Errorf("Authorize(single-string admin claim): want nil, got %v", err)
	}
}

func TestGroupClaimAuthorizer_CustomClaimName(t *testing.T) {
	az := &GroupClaimAuthorizer{
		ClaimName:       "roles",
		VerbGroupPolicy: map[string][]string{"mgmt.read.repo": {"reader-role"}},
	}
	id := &Identity{
		Subject:   "grace@example.com",
		RawClaims: map[string]json.RawMessage{"roles": json.RawMessage(`["reader-role"]`)},
	}
	if err := az.Authorize(context.Background(), id, "mgmt.read.repo"); err != nil {
		t.Errorf("Authorize(custom claim name `roles`): want nil, got %v", err)
	}
}

func TestGroupClaimAuthorizer_RejectsMalformedClaim(t *testing.T) {
	az := NewGroupClaimAuthorizer("")
	id := &Identity{
		Subject:   "henry@example.com",
		RawClaims: map[string]json.RawMessage{"groups": json.RawMessage(`{"nested": true}`)},
	}
	err := az.Authorize(context.Background(), id, "mgmt.read.repo")
	if err == nil {
		t.Fatalf("Authorize(object claim): want error, got nil")
	}
	if !errors.Is(err, ErrInsufficientGroup) {
		t.Errorf("Authorize: want ErrInsufficientGroup wrap, got %v", err)
	}
}

func TestDefaultVerbGroupPolicy_CoversEveryCanonicalVerb(t *testing.T) {
	policy := DefaultVerbGroupPolicy()
	for _, v := range CanonicalVerbs {
		dotted := v.DottedName()
		groups, ok := policy[dotted]
		if !ok {
			t.Errorf("default policy missing entry for canonical verb %q", dotted)
			continue
		}
		if len(groups) == 0 {
			t.Errorf("default policy entry for %q has empty group set (would always deny)", dotted)
		}
	}
}

// TestGateway_AuthzDenial_Returns403WithInsufficientGroup
// exercises the full HTTP pipeline: a valid bearer token
// from a low-privilege caller MUST be refused with 403 +
// WWW-Authenticate insufficient_scope + the canonical
// CodeInsufficientGroup body code.
func TestGateway_AuthzDenial_Returns403WithInsufficientGroup(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := &StaticHMACAuthenticator{
		Secret:   testSecret,
		Issuer:   issuerLiteral,
		Audience: audienceLiteral,
		Now:      func() time.Time { return now },
		Leeway:   30 * time.Second,
	}
	registry := NewVerbRegistry()
	downstreamCalled := false
	registry.Register(Verb{
		Namespace: "mgmt",
		Name:      "read.repo",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			downstreamCalled = true
			w.WriteHeader(http.StatusOK)
		}),
	})
	authz := NewGroupClaimAuthorizer("")
	gw := NewGatewayHandlerWithAuthorizer(auth, authz, registry, NoopTracer{}, nil)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	// Token verifies but carries only the CI group; the
	// canonical mgmt.read.repo verb requires readers or
	// admins, so authz must deny.
	tok := MintHS256TestToken(testSecret, map[string]any{
		"sub":    "ci-bot@example.com",
		"iss":    issuerLiteral,
		"aud":    audienceLiteral,
		"exp":    now.Add(time.Hour).Unix(),
		"groups": []string{GroupCI},
	})
	req, _ := http.NewRequest("GET", srv.URL+"/v1/mgmt/read.repo?repo_id=00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status=%d want %d", resp.StatusCode, http.StatusForbidden)
	}
	if downstreamCalled {
		t.Errorf("downstream handler called -- authz did not gate")
	}
	got := resp.Header.Get("WWW-Authenticate")
	if !strings.Contains(got, "insufficient_scope") {
		t.Errorf("WWW-Authenticate=%q want substring insufficient_scope", got)
	}
	var env errorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if env.Code != CodeInsufficientGroup {
		t.Errorf("code=%q want %q", env.Code, CodeInsufficientGroup)
	}
}

// TestGateway_AuthzAdmits_DownstreamReceivesRequest asserts
// the happy path: a caller carrying a matching group is
// admitted and the downstream verb handler receives the
// request with the canonical X-OIDC-Subject set.
func TestGateway_AuthzAdmits_DownstreamReceivesRequest(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := &StaticHMACAuthenticator{
		Secret:   testSecret,
		Issuer:   issuerLiteral,
		Audience: audienceLiteral,
		Now:      func() time.Time { return now },
		Leeway:   30 * time.Second,
	}
	registry := NewVerbRegistry()
	var capturedSubject string
	registry.Register(Verb{
		Namespace: "mgmt",
		Name:      "read.repo",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedSubject = r.Header.Get(OIDCSubjectHeader)
			w.WriteHeader(http.StatusOK)
		}),
	})
	authz := NewGroupClaimAuthorizer("")
	gw := NewGatewayHandlerWithAuthorizer(auth, authz, registry, NoopTracer{}, nil)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	tok := MintHS256TestToken(testSecret, map[string]any{
		"sub":    "alice@example.com",
		"iss":    issuerLiteral,
		"aud":    audienceLiteral,
		"exp":    now.Add(time.Hour).Unix(),
		"groups": []string{GroupReaders},
	})
	req, _ := http.NewRequest("GET", srv.URL+"/v1/mgmt/read.repo?repo_id=00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200", resp.StatusCode)
	}
	if capturedSubject != "alice@example.com" {
		t.Errorf("X-OIDC-Subject=%q want %q", capturedSubject, "alice@example.com")
	}
}

// TestGateway_NewGatewayHandler_NoAuthorizerInstallsNoop
// confirms the legacy 4-arg constructor still installs a
// permissive Authorizer so existing call sites stay
// backward-compatible.
func TestGateway_NewGatewayHandler_NoAuthorizerInstallsNoop(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	auth := &StaticHMACAuthenticator{
		Secret:   testSecret,
		Issuer:   issuerLiteral,
		Audience: audienceLiteral,
		Now:      func() time.Time { return now },
		Leeway:   30 * time.Second,
	}
	registry := NewVerbRegistry()
	registry.Register(Verb{
		Namespace: "mgmt",
		Name:      "read.repo",
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	})
	// 4-arg legacy constructor.
	gw := NewGatewayHandler(auth, registry, NoopTracer{}, nil)
	srv := httptest.NewServer(gw)
	defer srv.Close()

	// NO `groups` claim -- a NoopAuthorizer must still
	// admit this.
	tok := MintHS256TestToken(testSecret, map[string]any{
		"sub": "alice@example.com",
		"iss": issuerLiteral,
		"aud": audienceLiteral,
		"exp": now.Add(time.Hour).Unix(),
	})
	req, _ := http.NewRequest("GET", srv.URL+"/v1/mgmt/read.repo?repo_id=00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200 (NoopAuthorizer must admit)", resp.StatusCode)
	}
}

func TestGateway_NewGatewayHandlerWithAuthorizer_NilAuthzPanics(t *testing.T) {
	defer func() {
		if rec := recover(); rec == nil {
			t.Errorf("NewGatewayHandlerWithAuthorizer(nil authz): want panic, got nil")
		}
	}()
	auth := &StaticHMACAuthenticator{
		Secret:   testSecret,
		Audience: audienceLiteral,
	}
	_ = NewGatewayHandlerWithAuthorizer(auth, nil, NewVerbRegistry(), NoopTracer{}, nil)
}

// TestServerConfig_AuthorizerNilDefaultsToNoop confirms
// that a composition root which doesn't set Authorizer gets
// a permissive default (matches the legacy NewGatewayHandler
// behaviour). This isolates the authn-only test path from
// the authz-policy test path so existing server_test.go
// table tests still pass without `groups` claims.
func TestServerConfig_AuthorizerNilDefaultsToNoop(t *testing.T) {
	now := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	registry := NewVerbRegistry()
	registry.Register(Verb{
		Namespace: "mgmt",
		Name:      "read.repo",
		Handler:   http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
	})
	cfg := ServerConfig{
		Authenticator: &StaticHMACAuthenticator{
			Secret:   testSecret,
			Issuer:   issuerLiteral,
			Audience: audienceLiteral,
			Now:      func() time.Time { return now },
			Leeway:   30 * time.Second,
		},
		Registry: registry,
		// Authorizer: nil -- expect NoopAuthorizer default.
	}
	server := NewServer(cfg)
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()

	tok := MintHS256TestToken(testSecret, map[string]any{
		"sub": "alice@example.com",
		"iss": issuerLiteral,
		"aud": audienceLiteral,
		"exp": now.Add(time.Hour).Unix(),
	})
	req, _ := http.NewRequest("GET", srv.URL+"/v1/mgmt/read.repo?repo_id=00000000-0000-0000-0000-000000000000", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status=%d want 200 (nil Authorizer must default to Noop)", resp.StatusCode)
	}
}

func TestAdmittedGroups_ReturnsIntersection(t *testing.T) {
	caller := []string{GroupCI, GroupReaders, "extra"}
	required := []string{GroupReaders, GroupAdmins}
	got := AdmittedGroups(caller, required)
	if len(got) != 1 || got[0] != GroupReaders {
		t.Errorf("AdmittedGroups: got %v want [%q]", got, GroupReaders)
	}
}

func TestHasGroupIntersection_EmptyCases(t *testing.T) {
	if hasGroupIntersection(nil, []string{GroupAdmins}) {
		t.Errorf("nil caller: want false")
	}
	if hasGroupIntersection([]string{GroupAdmins}, nil) {
		t.Errorf("nil required: want false")
	}
	if hasGroupIntersection(nil, nil) {
		t.Errorf("both nil: want false")
	}
}

// identityWithGroups constructs an Identity whose RawClaims
// carry the supplied groups under the "groups" claim. Used
// by the table-driven tests above to avoid hand-encoding
// JSON in every assertion.
func identityWithGroups(t *testing.T, subject string, groups []string) *Identity {
	t.Helper()
	raw, err := json.Marshal(groups)
	if err != nil {
		t.Fatalf("json.Marshal(groups): %v", err)
	}
	return &Identity{
		Subject:   subject,
		RawClaims: map[string]json.RawMessage{"groups": raw},
	}
}
