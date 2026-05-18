package main

// Config-loading tests for the mgmt-api composition root. The
// HTTP / DB path is exercised by the unit tests in
// internal/mgmtapi/handler_unit_test.go; this file covers
// just the env-to-config glue.

import (
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestLoadConfig_missingPGURL(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":          "",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":  "tok",
		"AGENT_MEMORY_ALLOW_PLAINTEXT": "true",
	})
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "AGENT_MEMORY_PG_URL") {
		t.Fatalf("err = %v, want substring AGENT_MEMORY_PG_URL", err)
	}
}

// TestLoadConfig_noAuthenticator -- if NEITHER an OIDC trio
// (issuer + audience + jwks_url) NOR a dev token is set, the
// binary must refuse to start. Otherwise a fresh deployment
// would silently serve writes with a wide-open auth tier.
func TestLoadConfig_noAuthenticator(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":          "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":  "",
		"AGENT_MEMORY_OIDC_ISSUER":     "",
		"AGENT_MEMORY_OIDC_AUDIENCE":   "",
		"AGENT_MEMORY_OIDC_JWKS_URL":   "",
		"AGENT_MEMORY_ALLOW_PLAINTEXT": "true",
	})
	_, err := loadConfig()
	if err == nil {
		t.Fatalf("err = nil, want a no-authenticator error")
	}
	if !strings.Contains(err.Error(), "no authenticator configured") {
		t.Fatalf("err = %v, want substring 'no authenticator configured'", err)
	}
	if !strings.Contains(err.Error(), "AGENT_MEMORY_OIDC_DEV_TOKEN") {
		t.Fatalf("err = %v, want substring AGENT_MEMORY_OIDC_DEV_TOKEN", err)
	}
}

// TestLoadConfig_partialOIDCFallsBackToDevToken -- the OIDC
// trio is treated atomically: if any one of issuer / audience
// / jwks_url is missing, OIDC is not selected and the dev
// token is used (if available). This prevents an operator
// from accidentally believing OIDC is in force when only two
// of three vars are set.
func TestLoadConfig_partialOIDCFallsBackToDevToken(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":          "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_ISSUER":     "https://example.com",
		"AGENT_MEMORY_OIDC_AUDIENCE":   "mgmt-api",
		// JWKS URL missing -- triggers fallback.
		"AGENT_MEMORY_OIDC_JWKS_URL":   "",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":  "tok",
		"AGENT_MEMORY_ALLOW_PLAINTEXT": "true",
	})
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if c.AuthMode != "dev_static" {
		t.Errorf("AuthMode = %q, want dev_static", c.AuthMode)
	}
}

// TestLoadConfig_oidcModeWhenTrioConfigured -- when issuer,
// audience and jwks_url are all set the binary selects the
// real OIDC verifier even if AGENT_MEMORY_OIDC_DEV_TOKEN is
// also set. This makes "promote dev to prod" a single env-var
// flip with no possibility of the dev token being honored in
// production by mistake.
func TestLoadConfig_oidcModeWhenTrioConfigured(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":          "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_ISSUER":     "https://issuer.example/",
		"AGENT_MEMORY_OIDC_AUDIENCE":   "mgmt-api",
		"AGENT_MEMORY_OIDC_JWKS_URL":   "https://issuer.example/jwks",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":  "should-be-ignored",
		"AGENT_MEMORY_ALLOW_PLAINTEXT": "true",
	})
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if c.AuthMode != "oidc" {
		t.Errorf("AuthMode = %q, want oidc", c.AuthMode)
	}
}

// TestLoadConfig_oidcLeewayAndTTL -- optional OIDC tuning
// vars round-trip into the config. Empty defaults stay at
// mgmtapi.DefaultJWKSCacheTTL.
func TestLoadConfig_oidcLeewayAndTTL(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":          "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_ISSUER":     "https://issuer.example/",
		"AGENT_MEMORY_OIDC_AUDIENCE":   "mgmt-api",
		"AGENT_MEMORY_OIDC_JWKS_URL":   "https://issuer.example/jwks",
		"AGENT_MEMORY_OIDC_LEEWAY":     "5s",
		"AGENT_MEMORY_OIDC_JWKS_TTL":   "10m",
		"AGENT_MEMORY_ALLOW_PLAINTEXT": "true",
	})
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if c.OIDCLeeway.String() != "5s" {
		t.Errorf("OIDCLeeway = %v, want 5s", c.OIDCLeeway)
	}
	if c.OIDCJWKSTTL.String() != "10m0s" {
		t.Errorf("OIDCJWKSTTL = %v, want 10m0s", c.OIDCJWKSTTL)
	}
}

// TestLoadConfig_defaultResolverIsGitLsRemote -- with no
// AGENT_MEMORY_HEAD_RESOLVER override the binary selects the
// real `git ls-remote` resolver. Critically, the static SHA
// is NOT required -- prior iterations of this stage required
// AGENT_MEMORY_HEAD_RESOLVER_STATIC_SHA on every deployment,
// which encouraged operators to wire a fake SHA in prod.
func TestLoadConfig_defaultResolverIsGitLsRemote(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":          "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":  "tok",
		// Note: no AGENT_MEMORY_HEAD_RESOLVER, no STATIC_SHA.
		"AGENT_MEMORY_ALLOW_PLAINTEXT": "true",
	})
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if c.ResolverMode != "git-ls-remote" {
		t.Errorf("ResolverMode = %q, want git-ls-remote", c.ResolverMode)
	}
	if c.HeadResolverTimeout <= 0 {
		t.Errorf("HeadResolverTimeout = %v, want >0", c.HeadResolverTimeout)
	}
}

// TestLoadConfig_staticResolverRequiresSHA -- when the
// operator explicitly opts into the static resolver, the SHA
// is mandatory and must be a valid hex git SHA.
func TestLoadConfig_staticResolverRequiresSHA(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":                   "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":           "tok",
		"AGENT_MEMORY_HEAD_RESOLVER":            "static",
		"AGENT_MEMORY_HEAD_RESOLVER_STATIC_SHA": "",
		"AGENT_MEMORY_ALLOW_PLAINTEXT":          "true",
	})
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "HEAD_RESOLVER_STATIC_SHA") {
		t.Fatalf("err = %v, want substring HEAD_RESOLVER_STATIC_SHA", err)
	}
}

// TestLoadConfig_staticResolverRejectsMalformedSHA -- when
// the SHA is set but is not a hex git SHA the binary refuses
// to start.
func TestLoadConfig_staticResolverRejectsMalformedSHA(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":                   "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":           "tok",
		"AGENT_MEMORY_HEAD_RESOLVER":            "static",
		"AGENT_MEMORY_HEAD_RESOLVER_STATIC_SHA": "not-a-sha",
		"AGENT_MEMORY_ALLOW_PLAINTEXT":          "true",
	})
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "HEAD_RESOLVER_STATIC_SHA") {
		t.Fatalf("err = %v, want substring HEAD_RESOLVER_STATIC_SHA", err)
	}
}

func TestLoadConfig_unknownResolverMode(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":          "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":  "tok",
		"AGENT_MEMORY_HEAD_RESOLVER":   "bogus",
		"AGENT_MEMORY_ALLOW_PLAINTEXT": "true",
	})
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "bogus") {
		t.Fatalf("err = %v, want substring 'bogus'", err)
	}
}

func TestLoadConfig_requiresTLSFilesUnlessPlaintext(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":          "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":  "tok",
		"AGENT_MEMORY_ALLOW_PLAINTEXT": "",
	})
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "TLS_CERT_FILE") {
		t.Fatalf("err = %v, want substring TLS_CERT_FILE", err)
	}
}

func TestLoadConfig_okPlaintext(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":           "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":   "tok",
		"AGENT_MEMORY_OIDC_DEV_SUBJECT": "ops",
		"AGENT_MEMORY_ALLOW_PLAINTEXT":  "true",
		"AGENT_MEMORY_LISTEN_ADDR":      ":9999",
	})
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if c.ListenAddr != ":9999" {
		t.Errorf("ListenAddr = %q, want :9999", c.ListenAddr)
	}
	if c.OIDCDevSubject != "ops" {
		t.Errorf("OIDCDevSubject = %q, want ops", c.OIDCDevSubject)
	}
	if !c.AllowPlaintext {
		t.Errorf("AllowPlaintext = false, want true")
	}
	if c.AuthMode != "dev_static" {
		t.Errorf("AuthMode = %q, want dev_static", c.AuthMode)
	}
	if c.ResolverMode != "git-ls-remote" {
		t.Errorf("ResolverMode = %q, want git-ls-remote (default)", c.ResolverMode)
	}
}

func TestLoadConfig_okDefaultListenAddr(t *testing.T) {
	withEnv(t, map[string]string{
		"AGENT_MEMORY_PG_URL":          "postgres://localhost/x",
		"AGENT_MEMORY_OIDC_DEV_TOKEN":  "tok",
		"AGENT_MEMORY_ALLOW_PLAINTEXT": "true",
		"AGENT_MEMORY_LISTEN_ADDR":     "",
	})
	c, err := loadConfig()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if c.ListenAddr != ":8444" {
		t.Errorf("ListenAddr = %q, want :8444 (default)", c.ListenAddr)
	}
}

// TestBuildVerifier_devStaticEmitsStaticBearer -- the dev
// path must wire StaticBearerVerifier and log a WARN.
func TestBuildVerifier_devStaticEmitsStaticBearer(t *testing.T) {
	cfg := config{
		AuthMode:       "dev_static",
		OIDCDevToken:   "tok",
		OIDCDevSubject: "ops",
	}
	v, err := buildVerifier(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if v == nil {
		t.Fatalf("verifier = nil, want non-nil")
	}
}

// TestBuildResolver_gitLsRemoteIsDefault -- default resolver
// mode wires the real git ls-remote resolver.
func TestBuildResolver_gitLsRemoteIsDefault(t *testing.T) {
	cfg := config{ResolverMode: "git-ls-remote"}
	r, err := buildResolver(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if r == nil {
		t.Fatalf("resolver = nil, want non-nil")
	}
}

// TestBuildResolver_staticOptIn -- static mode opts the
// operator into the dev resolver explicitly.
func TestBuildResolver_staticOptIn(t *testing.T) {
	cfg := config{
		ResolverMode:          "static",
		HeadResolverStaticSHA: validSHA(),
	}
	r, err := buildResolver(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if r == nil {
		t.Fatalf("resolver = nil, want non-nil")
	}
}

// -----------------------------------------------------------
// Span ingest verb wiring (Stage 7.2).
// -----------------------------------------------------------

// TestParseSpanServiceMap_happyPath -- a single well-formed
// entry parses into a one-key map.
func TestParseSpanServiceMap_happyPath(t *testing.T) {
	m, err := parseSpanServiceMap("worker-a=550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got := m["worker-a"]; got != "550e8400-e29b-41d4-a716-446655440000" {
		t.Errorf("map[worker-a] = %q, want the supplied UUID", got)
	}
	if len(m) != 1 {
		t.Errorf("len(map) = %d, want 1", len(m))
	}
}

// TestParseSpanServiceMap_multiEntry_tolerantOfSpaces_and_trailingComma --
// real ops use comma-separated lists that may grow trailing
// whitespace or commas; the parser must accept those without
// failing the boot.
func TestParseSpanServiceMap_multiEntry_tolerantOfSpaces_and_trailingComma(t *testing.T) {
	in := " worker-a = repo-1 , worker-b = repo-2 ,"
	m, err := parseSpanServiceMap(in)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if m["worker-a"] != "repo-1" || m["worker-b"] != "repo-2" {
		t.Errorf("map = %+v, want {worker-a:repo-1, worker-b:repo-2}", m)
	}
}

// TestParseSpanServiceMap_malformed -- operator typos must
// fail the boot loudly; silently dropping spans for a typo
// would be a Sev2 surprise in prod.
func TestParseSpanServiceMap_malformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"no equals", "worker-a"},
		{"empty key", "=repo-1"},
		{"empty value", "worker-a="},
		{"duplicate key", "worker-a=repo-1,worker-a=repo-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseSpanServiceMap(tc.in); err == nil {
				t.Errorf("parseSpanServiceMap(%q) err = nil, want non-nil", tc.in)
			}
		})
	}
}

// TestStaticSpanLookup_emptyMap_returnsEmpty -- the
// nil/empty map case must return "" for every name so the
// handler classifies the call as unknown_service rather than
// panicking on a nil dereference.
func TestStaticSpanLookup_emptyMap_returnsEmpty(t *testing.T) {
	lk := staticSpanLookup(nil)
	if got := lk("worker-a"); got != "" {
		t.Errorf("lk(worker-a) = %q, want empty string", got)
	}
}

// TestStaticSpanLookup_defensiveCopy -- mutating the input
// map after closure construction must NOT affect the
// closure; in-flight requests would otherwise observe a
// torn map mid-call.
func TestStaticSpanLookup_defensiveCopy(t *testing.T) {
	src := map[string]string{"worker-a": "repo-1"}
	lk := staticSpanLookup(src)
	src["worker-a"] = "MUTATED"
	src["worker-b"] = "INJECTED"
	if got := lk("worker-a"); got != "repo-1" {
		t.Errorf("lk(worker-a) = %q, want repo-1 (mutation leaked into closure)", got)
	}
	if got := lk("worker-b"); got != "" {
		t.Errorf("lk(worker-b) = %q, want empty (injection leaked into closure)", got)
	}
}

// TestBuildSpanForwarder_emptyURL_returnsNil -- when the
// forward URL is unset the helper returns nil so NewHandler
// installs its fail-CLOSED default. A previous draft returned
// a silent no-op here, which would have 202'd while dropping
// every span -- caught by design review and explicitly tested
// here so the regression can never sneak back in.
func TestBuildSpanForwarder_emptyURL_returnsNil(t *testing.T) {
	cfg := config{SpanForwardURL: ""}
	f := buildSpanForwarder(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if f != nil {
		t.Fatalf("forwarder = %T, want nil so the handler installs its fail-closed default", f)
	}
}

// TestBuildSpanForwarder_setURL_returnsHTTPForwarder -- the
// happy path returns a real HTTP forwarder configured with
// the URL + timeout from config.
func TestBuildSpanForwarder_setURL_returnsHTTPForwarder(t *testing.T) {
	cfg := config{
		SpanForwardURL:     "https://span-ingestor.local:4318/v1/traces",
		SpanForwardTimeout: 5_000_000_000, // 5s
	}
	f := buildSpanForwarder(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if f == nil {
		t.Fatalf("forwarder = nil, want non-nil")
	}
}

// validSHA returns a 40-char lower-case hex string suitable
// for AGENT_MEMORY_HEAD_RESOLVER_STATIC_SHA.
func validSHA() string {
	return "abcdefabcdefabcdefabcdefabcdefabcdef0001"
}

// withEnv installs `kv` for the duration of the test and
// restores the previous values on cleanup. Empty values cause
// the variable to be UNSET (so the absence-of-value branches
// of loadConfig are exercisable).
func withEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for k, v := range kv {
		t.Setenv(k, v)
		if v == "" {
			// t.Setenv with "" sets the var to empty
			// string, which loadConfig treats as
			// unset. Both branches are equivalent for
			// these tests.
			_ = v
		}
	}
}
