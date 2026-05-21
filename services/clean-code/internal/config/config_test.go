package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// clearCleanCodeEnv unsets every CLEAN_CODE_* env var the loader
// reads so a stale value in the test runner's environment cannot
// leak into the next subtest.
func clearCleanCodeEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		EnvConfigFile,
		EnvASTModeDefault,
		EnvExternalMetricCoverageFormat,
		EnvGateDegradedPolicy,
		EnvPolicySigningRequired,
		EnvRefactorEffortSource,
		EnvHTTPAddr,
		EnvPrometheusAddr,
		EnvOTelEndpoint,
		EnvPGURL,
		EnvLogLevel,
		EnvScanTimeout,
		EnvPeriodicSweepCadence,
		EnvWindowDays,
		EnvFreshnessWindowSeconds,
		EnvPolicyPublishOverlapSeconds,
	} {
		t.Setenv(k, "")
	}
}

// TestDefaults_OperatorPins is the load-bearing assertion for
// implementation-plan.md Stage 1.1 acceptance scenario
// `config-honours-pins`: omitting the five operator pins must
// return defaults matching architecture Sec 1.6 exactly. Any
// drift in these string defaults (e.g. "Cobertura" vs
// "Cobertura XML") flips a downstream ingest webhook into the
// wrong payload-format branch, so the literals here are pinned
// against the architecture doc.
func TestDefaults_OperatorPins(t *testing.T) {
	t.Parallel()
	d := Defaults()
	cases := map[string]struct {
		got, want string
	}{
		"ast-mode-default":                {d.ASTModeDefault, "embedded"},
		"external-metric-coverage-format": {d.ExternalMetricCoverageFormat, "Cobertura XML"},
		"gate-degraded-policy":            {d.GateDegradedPolicy, "warn"},
		"policy-signing-required":         {d.PolicySigningRequired, "v1 required"},
		"refactor-effort-source":          {d.RefactorEffortSource, "ML model from historical commits"},
	}
	for pin, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("operator pin %s: got %q, want %q (architecture Sec 1.6)", pin, tc.got, tc.want)
		}
	}
}

// TestLoad_NoEnvReturnsDefaults verifies the `config-honours-pins`
// scenario at the loader boundary: with no env vars set, Load()
// must return a Config whose five operator pins match the
// architecture Sec 1.6 defaults verbatim.
func TestLoad_NoEnvReturnsDefaults(t *testing.T) {
	clearCleanCodeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.ASTModeDefault != "embedded" {
		t.Errorf("ASTModeDefault = %q; want %q", cfg.ASTModeDefault, "embedded")
	}
	if cfg.ExternalMetricCoverageFormat != "Cobertura XML" {
		t.Errorf("ExternalMetricCoverageFormat = %q; want %q", cfg.ExternalMetricCoverageFormat, "Cobertura XML")
	}
	if cfg.GateDegradedPolicy != "warn" {
		t.Errorf("GateDegradedPolicy = %q; want %q", cfg.GateDegradedPolicy, "warn")
	}
	if cfg.PolicySigningRequired != "v1 required" {
		t.Errorf("PolicySigningRequired = %q; want %q", cfg.PolicySigningRequired, "v1 required")
	}
	if cfg.RefactorEffortSource != "ML model from historical commits" {
		t.Errorf("RefactorEffortSource = %q; want %q", cfg.RefactorEffortSource, "ML model from historical commits")
	}
}

// TestLoad_NumericDefaults verifies the tech-spec Sec 8.2 numeric
// defaults make it through Load.
func TestLoad_NumericDefaults(t *testing.T) {
	clearCleanCodeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.ScanTimeout != 30*time.Minute {
		t.Errorf("ScanTimeout = %s; want 30m", cfg.ScanTimeout)
	}
	if cfg.PeriodicSweepCadence != 5*time.Minute {
		t.Errorf("PeriodicSweepCadence = %s; want 5m", cfg.PeriodicSweepCadence)
	}
	if cfg.WindowDays != 90 {
		t.Errorf("WindowDays = %d; want 90", cfg.WindowDays)
	}
	if cfg.FreshnessWindowSeconds != 3600 {
		t.Errorf("FreshnessWindowSeconds = %d; want 3600", cfg.FreshnessWindowSeconds)
	}
	if cfg.PolicyPublishOverlapSeconds != 86400 {
		t.Errorf("PolicyPublishOverlapSeconds = %d; want 86400", cfg.PolicyPublishOverlapSeconds)
	}
}

func TestLoad_EnvOverridesPins(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvASTModeDefault, "linked")
	t.Setenv(EnvGateDegradedPolicy, "block")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.ASTModeDefault != "linked" {
		t.Errorf("ASTModeDefault = %q; want %q", cfg.ASTModeDefault, "linked")
	}
	if cfg.GateDegradedPolicy != "block" {
		t.Errorf("GateDegradedPolicy = %q; want %q", cfg.GateDegradedPolicy, "block")
	}
	// untouched pins keep their defaults
	if cfg.ExternalMetricCoverageFormat != "Cobertura XML" {
		t.Errorf("ExternalMetricCoverageFormat regressed to %q", cfg.ExternalMetricCoverageFormat)
	}
}

func TestLoad_RejectsInvalidASTMode(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvASTModeDefault, "monolith")
	if _, err := Load(); err == nil {
		t.Fatalf("Load: want error for invalid ast-mode-default; got nil")
	} else if !strings.Contains(err.Error(), "ast-mode-default") {
		t.Fatalf("Load: error %q; want it to mention ast-mode-default", err)
	}
}

func TestLoad_RejectsInvalidGatePolicy(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvGateDegradedPolicy, "ignore")
	if _, err := Load(); err == nil {
		t.Fatalf("Load: want error for invalid gate-degraded-policy; got nil")
	} else if !strings.Contains(err.Error(), "gate-degraded-policy") {
		t.Fatalf("Load: error %q; want it to mention gate-degraded-policy", err)
	}
}

func TestLoad_RejectsMalformedDuration(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvScanTimeout, "thirty-minutes")
	if _, err := Load(); err == nil {
		t.Fatalf("Load: want ParseDuration error; got nil")
	} else if !strings.Contains(err.Error(), EnvScanTimeout) {
		t.Fatalf("Load: error %q; want it to mention %s", err, EnvScanTimeout)
	}
}

func TestLoad_RejectsMalformedInt(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvWindowDays, "ninety")
	if _, err := Load(); err == nil {
		t.Fatalf("Load: want strconv.Atoi error; got nil")
	} else if !strings.Contains(err.Error(), EnvWindowDays) {
		t.Fatalf("Load: error %q; want it to mention %s", err, EnvWindowDays)
	}
}

func TestLoad_RejectsNonPositiveWindowDays(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvWindowDays, "0")
	if _, err := Load(); err == nil {
		t.Fatalf("Load: want non-positive error; got nil")
	} else if !strings.Contains(err.Error(), "window-days") {
		t.Fatalf("Load: error %q; want it to mention window-days", err)
	}
}

// TestLoad_ConfigFileAndEnvCompose verifies the file -> env
// precedence: a file sets one pin, env overrides another, the
// remaining pins fall back to architecture defaults.
func TestLoad_ConfigFileAndEnvCompose(t *testing.T) {
	clearCleanCodeEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "clean-code.conf")
	contents := strings.Join([]string{
		"# stage 1.1 scaffold test fixture",
		EnvASTModeDefault + "=linked",
		EnvHTTPAddr + "=:9000",
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	t.Setenv(EnvConfigFile, path)
	t.Setenv(EnvGateDegradedPolicy, "block") // env overrides file (file did not set it)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.ASTModeDefault != "linked" {
		t.Errorf("ASTModeDefault (file): %q; want %q", cfg.ASTModeDefault, "linked")
	}
	if cfg.HTTPAddr != ":9000" {
		t.Errorf("HTTPAddr (file): %q; want %q", cfg.HTTPAddr, ":9000")
	}
	if cfg.GateDegradedPolicy != "block" {
		t.Errorf("GateDegradedPolicy (env): %q; want %q", cfg.GateDegradedPolicy, "block")
	}
	if cfg.PolicySigningRequired != "v1 required" {
		t.Errorf("PolicySigningRequired (default): %q; want %q", cfg.PolicySigningRequired, "v1 required")
	}
}

func TestLoad_ConfigFileEnvOverridesFile(t *testing.T) {
	clearCleanCodeEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "clean-code.conf")
	if err := os.WriteFile(path, []byte(EnvASTModeDefault+"=embedded\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	t.Setenv(EnvConfigFile, path)
	t.Setenv(EnvASTModeDefault, "linked")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.ASTModeDefault != "linked" {
		t.Errorf("env should override file: got %q, want %q", cfg.ASTModeDefault, "linked")
	}
}

func TestLoad_RejectsUnknownConfigFileKey(t *testing.T) {
	clearCleanCodeEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "clean-code.conf")
	if err := os.WriteFile(path, []byte("CLEAN_CODE_NOT_A_REAL_KEY=oops\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	t.Setenv(EnvConfigFile, path)
	if _, err := Load(); err == nil {
		t.Fatalf("Load: want unknown-key error; got nil")
	} else if !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("Load: error %q; want it to mention unknown config key", err)
	}
}

func TestLoad_MissingConfigFileSurfaces(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvConfigFile, "/this/path/does/not/exist.conf")
	if _, err := Load(); err == nil {
		t.Fatalf("Load: want missing-file error; got nil")
	}
}

func TestValidate_RejectsBadLogLevel(t *testing.T) {
	cfg := Defaults()
	cfg.LogLevel = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatalf("Validate: want error for log-level=verbose; got nil")
	}
}
