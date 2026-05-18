package main

// Config-loading tests for the mgmt-api composition root. The
// HTTP / DB path is exercised by the unit tests in
// internal/mgmtapi/handler_unit_test.go; this file covers
// just the env-to-config glue.

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/snapshot"
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

// TestWriteSnapshotMetrics_zeroState verifies the /metrics
// exposition format when no enqueues have happened yet.
// Both counters MUST appear (Prometheus scrapers expect a
// stable metric surface even when values are 0). Iter-2
// fix #4 acceptance gate.
func TestWriteSnapshotMetrics_zeroState(t *testing.T) {
	buf := &bytes.Buffer{}
	writeSnapshotMetrics(buf, snapshot.NewMetrics())
	body := buf.String()
	for _, want := range []string{
		"# HELP " + snapshot.MetricSnapshotPendingTotal,
		"# TYPE " + snapshot.MetricSnapshotPendingTotal + " counter",
		snapshot.MetricSnapshotPendingTotal + " 0",
		"# HELP " + snapshot.MetricSnapshotPublishedTotal,
		"# TYPE " + snapshot.MetricSnapshotPublishedTotal + " counter",
		snapshot.MetricSnapshotPublishedTotal + " 0",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestWriteSnapshotMetrics_pendingIncremented verifies the
// counter value actually flows through the exposition.
func TestWriteSnapshotMetrics_pendingIncremented(t *testing.T) {
	m := snapshot.NewMetrics()
	m.IncPending(5)
	m.IncPending(2)
	buf := &bytes.Buffer{}
	writeSnapshotMetrics(buf, m)
	body := buf.String()
	if !strings.Contains(body, snapshot.MetricSnapshotPendingTotal+" 7\n") {
		t.Errorf("expected '%s 7' in body, got:\n%s",
			snapshot.MetricSnapshotPendingTotal, body)
	}
}

// TestWriteSnapshotMetrics_nilMetrics is the defence-in-depth
// guard: a /metrics scrape on a binary that didn't wire up
// snapshot must still 200 instead of nil-deref panicking.
func TestWriteSnapshotMetrics_nilMetrics(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("writeSnapshotMetrics(nil) panicked: %v", r)
		}
	}()
	buf := &bytes.Buffer{}
	writeSnapshotMetrics(buf, nil)
	body := buf.String()
	if !strings.Contains(body, snapshot.MetricSnapshotPendingTotal+" 0") {
		t.Errorf("nil-Metrics body did not expose pending counter: %s", body)
	}
}

// TestWriteSnapshotMetrics_orderingDeterministic verifies the
// fixed exposition order so Prometheus scrape diffs stay
// human-readable.
func TestWriteSnapshotMetrics_orderingDeterministic(t *testing.T) {
	m := snapshot.NewMetrics()
	m.IncPending(1)
	render := func() string {
		b := &bytes.Buffer{}
		writeSnapshotMetrics(b, m)
		return b.String()
	}
	a, b := render(), render()
	if a != b {
		t.Fatalf("writeSnapshotMetrics not deterministic:\nA:\n%s\nB:\n%s", a, b)
	}
	// pending must appear before published (federated query
	// builders rely on this).
	pendingIdx := strings.Index(a, snapshot.MetricSnapshotPendingTotal+" ")
	publishedIdx := strings.Index(a, snapshot.MetricSnapshotPublishedTotal+" ")
	if pendingIdx < 0 || publishedIdx < 0 || pendingIdx > publishedIdx {
		t.Errorf("ordering: pending @ %d, published @ %d (pending must come first)", pendingIdx, publishedIdx)
	}
}

// TestResolveSnapshotModelVersion pins the iter-3 operator
// answer "model-version-source: both-with-derive-as-default".
// Resolution order is:
//
//  1. Explicit `AGENT_MEMORY_EMBEDDING_MODEL_VERSION` wins.
//  2. Derive from stub when AGENT_MEMORY_ALLOW_STUB_EMBEDDER.
//  3. Empty → verb disabled.
//
// Regression test: a future change that flipped the
// resolution order (so the derived stub value silently
// overrode an operator-supplied explicit stamp) would be a
// production-safety bug the snapshot verb's model-bump
// runbook does not catch — that scenario is exactly what
// this test guards against.
func TestResolveSnapshotModelVersion(t *testing.T) {
	cases := []struct {
		name       string
		cfg        config
		wantStamp  string
		wantSource string
	}{
		{
			name:       "explicit_env_wins_over_stub",
			cfg:        config{EmbeddingModelVersion: "openai-3-small@v2", AllowStubEmbedder: true},
			wantStamp:  "openai-3-small@v2",
			wantSource: "explicit_env",
		},
		{
			name:       "explicit_env_no_stub",
			cfg:        config{EmbeddingModelVersion: "openai-3-small@v2"},
			wantStamp:  "openai-3-small@v2",
			wantSource: "explicit_env",
		},
		{
			name:       "derive_from_stub_when_env_empty",
			cfg:        config{AllowStubEmbedder: true},
			wantStamp:  "stub-zero-vector@v0",
			wantSource: "derived_stub_embedder",
		},
		{
			name:       "disabled_when_neither_configured",
			cfg:        config{},
			wantStamp:  "",
			wantSource: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStamp, gotSource := resolveSnapshotModelVersion(tc.cfg)
			if gotStamp != tc.wantStamp {
				t.Errorf("modelVersion = %q, want %q", gotStamp, tc.wantStamp)
			}
			if gotSource != tc.wantSource {
				t.Errorf("source = %q, want %q", gotSource, tc.wantSource)
			}
		})
	}
}

// TestParseAllowStubEmbedder is the focused unit test for the
// tolerant boolean-env parser. Production deployments MUST
// fail closed (return false) on unparseable input — a
// production binary that accidentally interpreted "yeah ok"
// as true would silently flip on the stub embedder.
func TestParseAllowStubEmbedder(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"true", true},
		{"True", true},
		{"1", true},
		{"false", false},
		{"0", false},
		{"", false},
		{"  ", false},
		{"yeah ok", false},
		{" true ", true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := parseAllowStubEmbedder(tc.in); got != tc.want {
				t.Errorf("parseAllowStubEmbedder(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
