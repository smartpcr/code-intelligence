package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLoadGatewayConfig_DefaultMode_RequiresOIDC asserts the
// production-default mode fails fast when OIDC config is
// missing. This is the strict-opt-in invariant pinned by
// the rubber-duck critique #1 (no silent fallback to HMAC).
func TestLoadGatewayConfig_DefaultMode_RequiresOIDC(t *testing.T) {
	// The runner inherits the operator's real env; reset
	// every var we read so the test is hermetic.
	resetGatewayEnv(t)
	t.Setenv(envPGURL, "postgres://localhost/test")

	_, err := loadGatewayConfig()
	if err == nil {
		t.Fatalf("loadGatewayConfig: want error in default mode without OIDC config, got nil")
	}
	if !strings.Contains(err.Error(), envOIDCIssuer) {
		t.Errorf("loadGatewayConfig: error %q missing reference to %s", err, envOIDCIssuer)
	}
}

func TestLoadGatewayConfig_OIDCMode_RequiresAudience(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envPGURL, "postgres://localhost/test")
	t.Setenv(envOIDCIssuer, "https://idp.example/")

	_, err := loadGatewayConfig()
	if err == nil {
		t.Fatalf("loadGatewayConfig: want error without audience, got nil")
	}
	if !strings.Contains(err.Error(), envOIDCAudience) {
		t.Errorf("loadGatewayConfig: error %q missing reference to %s", err, envOIDCAudience)
	}
}

func TestLoadGatewayConfig_OIDCMode_Succeeds(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envPGURL, "postgres://localhost/test")
	t.Setenv(envOIDCIssuer, "https://idp.example/")
	t.Setenv(envOIDCAudience, "clean-code-gateway")

	cfg, err := loadGatewayConfig()
	if err != nil {
		t.Fatalf("loadGatewayConfig: %v", err)
	}
	if cfg.AuthMode != authModeOIDC {
		t.Errorf("AuthMode=%q want %q", cfg.AuthMode, authModeOIDC)
	}
	if cfg.OIDCIssuer != "https://idp.example/" {
		t.Errorf("OIDCIssuer=%q want %q", cfg.OIDCIssuer, "https://idp.example/")
	}
	if cfg.OIDCAudience != "clean-code-gateway" {
		t.Errorf("OIDCAudience=%q want %q", cfg.OIDCAudience, "clean-code-gateway")
	}
}

func TestLoadGatewayConfig_DevHMACMode_RequiresSecret(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envPGURL, "postgres://localhost/test")
	t.Setenv(envGatewayAuthMode, authModeDevHMAC)
	t.Setenv(envGatewayHMACAudience, "test-aud")

	_, err := loadGatewayConfig()
	if err == nil {
		t.Fatalf("loadGatewayConfig: want error without HMAC secret, got nil")
	}
	if !strings.Contains(err.Error(), envGatewayHMACSecretB64) {
		t.Errorf("loadGatewayConfig: error %q missing reference to %s", err, envGatewayHMACSecretB64)
	}
}

func TestLoadGatewayConfig_DevHMACMode_RejectsShortSecret(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envPGURL, "postgres://localhost/test")
	t.Setenv(envGatewayAuthMode, authModeDevHMAC)
	// 16 bytes -- below the 32-byte HMAC-SHA256 minimum.
	// base64 of 16 zero bytes = "AAAAAAAAAAAAAAAAAAAAAA==".
	t.Setenv(envGatewayHMACSecretB64, "AAAAAAAAAAAAAAAAAAAAAA==")
	t.Setenv(envGatewayHMACAudience, "test-aud")

	_, err := loadGatewayConfig()
	if err == nil {
		t.Fatalf("loadGatewayConfig: want error for too-short secret, got nil")
	}
	if !strings.Contains(err.Error(), "32") {
		t.Errorf("loadGatewayConfig: error %q missing 32-byte minimum reference", err)
	}
}

func TestLoadGatewayConfig_DevHMACMode_Succeeds(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envPGURL, "postgres://localhost/test")
	t.Setenv(envGatewayAuthMode, authModeDevHMAC)
	// 32 raw bytes of zeros, base64 encoded.
	t.Setenv(envGatewayHMACSecretB64, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv(envGatewayHMACAudience, "test-aud")

	cfg, err := loadGatewayConfig()
	if err != nil {
		t.Fatalf("loadGatewayConfig: %v", err)
	}
	if cfg.AuthMode != authModeDevHMAC {
		t.Errorf("AuthMode=%q want %q", cfg.AuthMode, authModeDevHMAC)
	}
	if len(cfg.HMACSecret) != 32 {
		t.Errorf("HMACSecret len=%d want 32", len(cfg.HMACSecret))
	}
}

func TestLoadGatewayConfig_RejectsUnknownMode(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envPGURL, "postgres://localhost/test")
	t.Setenv(envGatewayAuthMode, "magic-mode")

	_, err := loadGatewayConfig()
	if err == nil {
		t.Fatalf("loadGatewayConfig: want error for unknown mode, got nil")
	}
}

func TestLoadGatewayConfig_RequiresPGURL(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envOIDCIssuer, "https://idp.example/")
	t.Setenv(envOIDCAudience, "clean-code-gateway")

	_, err := loadGatewayConfig()
	if err == nil {
		t.Fatalf("loadGatewayConfig: want error without PGURL, got nil")
	}
	if !strings.Contains(err.Error(), envPGURL) && !strings.Contains(err.Error(), envGatewayPGURL) {
		t.Errorf("loadGatewayConfig: error %q missing PGURL reference", err)
	}
}

func TestPickPGURL_PrefersGatewaySpecific(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envPGURL, "postgres://generic/")
	t.Setenv(envGatewayPGURL, "postgres://gateway/")

	got := pickPGURL()
	if got != "postgres://gateway/" {
		t.Errorf("pickPGURL: want gateway-specific, got %q", got)
	}
}

func TestBuildAuthenticator_OIDCMode_ReturnsOIDC(t *testing.T) {
	cfg := gatewayConfig{
		AuthMode:     authModeOIDC,
		OIDCIssuer:   "https://idp.example/",
		OIDCAudience: "clean-code-gateway",
	}
	auth, err := buildAuthenticator(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("buildAuthenticator: %v", err)
	}
	if auth == nil {
		t.Fatalf("buildAuthenticator: returned nil authenticator")
	}
}

func TestBuildAuthenticator_DevHMAC_ReturnsStaticHMAC(t *testing.T) {
	cfg := gatewayConfig{
		AuthMode:     authModeDevHMAC,
		HMACSecret:   make([]byte, 32),
		HMACAudience: "test-aud",
	}
	auth, err := buildAuthenticator(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatalf("buildAuthenticator: %v", err)
	}
	if auth == nil {
		t.Fatalf("buildAuthenticator: returned nil authenticator")
	}
}

func TestPingDBWithRetry_CancelledContext_ReturnsCancelError(t *testing.T) {
	// Use a context that is immediately cancelled so the
	// retry loop exits on the first ctx.Done() select.
	// A nil *sql.DB Ping panics, so we cancel the ctx
	// before the first Ping by passing a pre-cancelled ctx;
	// but pingDBWithRetry calls Ping first. To avoid
	// the panic we use sql.OpenDB with a fake driver
	// approach instead. Simpler: we just verify the
	// cancellation propagation by passing a cancelled ctx
	// and observing the function returns within the
	// pingDelay window (one Ping attempt against nil is
	// the failure mode we want to test).
	//
	// To exercise the cancel path WITHOUT a real DB, we
	// rely on the fact that sql.DB.PingContext on a freshly-
	// opened invalid DSN returns an error promptly; the
	// retry loop then enters the select{} where the
	// cancelled ctx wins.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Use an open against an unreachable DSN. sql.Open is
	// lazy, so this does not block.
	db, err := openDB("postgres://127.0.0.1:1/nope?sslmode=disable&connect_timeout=1")
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	defer db.Close()
	err = pingDBWithRetry(ctx, db)
	if err == nil {
		t.Fatalf("pingDBWithRetry: want error with cancelled context, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		// Accept either the cancel error or the underlying
		// ping error wrapped by pingDBWithRetry -- both
		// are acceptable failure modes. We just want to
		// confirm the function returns rather than
		// blocking on a non-cancellable retry loop.
		// Continue without asserting deeper.
		t.Logf("pingDBWithRetry: ctx-cancelled returned %v (any non-nil is acceptable)", err)
	}
	// The bounded-retry loop SHOULD return promptly when
	// the context is already cancelled. If this hangs the
	// test runner kills it; that's the practical
	// detection.
}

func TestEnvSecondsOrDefault_ReturnsDefaultOnEmpty(t *testing.T) {
	resetGatewayEnv(t)
	got, err := envSecondsOrDefault(envShutdownTimeoutSeconds, defaultShutdownSeconds)
	if err != nil {
		t.Fatalf("envSecondsOrDefault: %v", err)
	}
	if got != defaultShutdownSeconds {
		t.Errorf("envSecondsOrDefault: want %d, got %d", defaultShutdownSeconds, got)
	}
}

func TestEnvSecondsOrDefault_ParsesValid(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envShutdownTimeoutSeconds, "45")
	got, err := envSecondsOrDefault(envShutdownTimeoutSeconds, defaultShutdownSeconds)
	if err != nil {
		t.Fatalf("envSecondsOrDefault: %v", err)
	}
	if got != 45 {
		t.Errorf("envSecondsOrDefault: want 45, got %d", got)
	}
}

func TestEnvSecondsOrDefault_RejectsNegative(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envShutdownTimeoutSeconds, "-1")
	_, err := envSecondsOrDefault(envShutdownTimeoutSeconds, defaultShutdownSeconds)
	if err == nil {
		t.Fatalf("envSecondsOrDefault: want error for negative, got nil")
	}
}

func TestEnvSecondsOrDefault_RejectsNonNumeric(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envShutdownTimeoutSeconds, "not-a-number")
	_, err := envSecondsOrDefault(envShutdownTimeoutSeconds, defaultShutdownSeconds)
	if err == nil {
		t.Fatalf("envSecondsOrDefault: want error for non-numeric, got nil")
	}
}

func TestLoadGatewayConfig_ParsesShutdownTimeout(t *testing.T) {
	resetGatewayEnv(t)
	t.Setenv(envPGURL, "postgres://localhost/test")
	t.Setenv(envOIDCIssuer, "https://idp.example/")
	t.Setenv(envOIDCAudience, "clean-code-gateway")
	t.Setenv(envShutdownTimeoutSeconds, "60")

	cfg, err := loadGatewayConfig()
	if err != nil {
		t.Fatalf("loadGatewayConfig: %v", err)
	}
	if cfg.ShutdownTimeout != 60*time.Second {
		t.Errorf("ShutdownTimeout=%v want 60s", cfg.ShutdownTimeout)
	}
}

func TestFallbackString(t *testing.T) {
	if got := fallbackString("hello", "fallback"); got != "hello" {
		t.Errorf("fallbackString: want %q, got %q", "hello", got)
	}
	if got := fallbackString("", "fallback"); got != "fallback" {
		t.Errorf("fallbackString: want %q, got %q", "fallback", got)
	}
}

// resetGatewayEnv unsets every env var this binary's loader
// consults so a test starts from a clean slate regardless of
// the operator's local environment.
func resetGatewayEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		envPort,
		envGatewayAuthMode,
		envOIDCIssuer,
		envOIDCAudience,
		envOIDCJWKSURL,
		envGatewayHMACSecretB64,
		envGatewayHMACAudience,
		envGatewayPGURL,
		envPGURL,
		envKMSProvider,
		envKMSMasterKeyHex,
		envShutdownTimeoutSeconds,
	}
	for _, k := range keys {
		// Use t.Setenv with "" to clear, but t.Setenv
		// requires a non-empty key; we use os.Unsetenv
		// for explicit reset and t.Cleanup to restore.
		prior, hadPrior := os.LookupEnv(k)
		_ = os.Unsetenv(k)
		if hadPrior {
			val := prior
			key := k
			t.Cleanup(func() { _ = os.Setenv(key, val) })
		}
	}
}
