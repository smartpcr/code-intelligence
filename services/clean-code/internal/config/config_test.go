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
		EnvKMSProvider,
		EnvKMSMasterKeyHex,
		EnvWebhookHMACSecret,
		EnvEnableScaffoldChurnWebhook,
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

// TestKMSProvider_DefaultsAndClosedSet pins Stage 5.1
// composition-root behaviour: KMSProvider defaults to "" so
// scaffold-mode startup stays signing-disabled; the closed set
// is `{"", "local", "in-memory"}`; "local" requires a master
// key of exactly 64 hex chars; and a master key set with a
// non-local provider is rejected (fail-closed -- never silently
// drop the master).
func TestKMSProvider_DefaultsAndClosedSet(t *testing.T) {
	clearCleanCodeEnv(t)

	t.Run("default empty", func(t *testing.T) {
		clearCleanCodeEnv(t)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.KMSProvider != "" {
			t.Errorf("KMSProvider default = %q; want \"\"", cfg.KMSProvider)
		}
		if cfg.KMSMasterKeyHex != "" {
			t.Errorf("KMSMasterKeyHex default = %q; want \"\"", cfg.KMSMasterKeyHex)
		}
	})

	t.Run("unknown provider rejected", func(t *testing.T) {
		clearCleanCodeEnv(t)
		t.Setenv(EnvKMSProvider, "magic-vault")
		if _, err := Load(); err == nil {
			t.Error("Load with KMSProvider=magic-vault: err=nil; want closed-set rejection")
		}
	})

	t.Run("local requires master key", func(t *testing.T) {
		clearCleanCodeEnv(t)
		t.Setenv(EnvKMSProvider, "local")
		if _, err := Load(); err == nil {
			t.Error("Load with KMSProvider=local but empty master key: err=nil; want length check")
		}
	})

	t.Run("local master key wrong length", func(t *testing.T) {
		clearCleanCodeEnv(t)
		t.Setenv(EnvKMSProvider, "local")
		t.Setenv(EnvKMSMasterKeyHex, "deadbeef")
		if _, err := Load(); err == nil {
			t.Error("Load with short master key: err=nil; want length check")
		}
	})

	t.Run("master key without local provider rejected", func(t *testing.T) {
		clearCleanCodeEnv(t)
		t.Setenv(EnvKMSProvider, "in-memory")
		// 64 hex chars but provider is in-memory -- the
		// master key has nowhere to go. Fail-closed.
		t.Setenv(EnvKMSMasterKeyHex, strings.Repeat("a", 64))
		if _, err := Load(); err == nil {
			t.Error("Load with master key + in-memory provider: err=nil; want fail-closed rejection")
		}
	})

	t.Run("local valid round-trip", func(t *testing.T) {
		clearCleanCodeEnv(t)
		t.Setenv(EnvKMSProvider, "local")
		t.Setenv(EnvKMSMasterKeyHex, strings.Repeat("a", 64))
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load (local + 64-hex master): %v", err)
		}
		if cfg.KMSProvider != "local" {
			t.Errorf("KMSProvider = %q; want local", cfg.KMSProvider)
		}
		if len(cfg.KMSMasterKeyHex) != 64 {
			t.Errorf("KMSMasterKeyHex len = %d; want 64", len(cfg.KMSMasterKeyHex))
		}
	})

	t.Run("in-memory provider accepted with no master key", func(t *testing.T) {
		clearCleanCodeEnv(t)
		t.Setenv(EnvKMSProvider, "in-memory")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load (in-memory, no master): %v", err)
		}
		if cfg.KMSProvider != "in-memory" {
			t.Errorf("KMSProvider = %q; want in-memory", cfg.KMSProvider)
		}
	})
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

// --- Stage 2.6 iter 6: scaffold churn-webhook env interlock ---

// TestChurnWebhook_HMACEnvFields_DefaultsAreEmpty pins that
// neither env var is set by default. Production deployments
// that don't opt in must observe a fully unmounted webhook
// (evaluator iter-5 #3 -- scaffold-mode persistence loses data,
// so it must be off by default).
func TestChurnWebhook_HMACEnvFields_DefaultsAreEmpty(t *testing.T) {
	clearCleanCodeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.WebhookHMACSecret != "" {
		t.Errorf("WebhookHMACSecret: want empty, got %q", cfg.WebhookHMACSecret)
	}
	if cfg.EnableScaffoldChurnWebhook {
		t.Errorf("EnableScaffoldChurnWebhook: want false, got true")
	}
}

// TestChurnWebhook_HMACEnvFields_BothSetIsValid pins the
// "both-or-neither" interlock's accept-both branch: setting
// EnableScaffoldChurnWebhook=true AND a non-empty HMAC secret
// is the valid scaffold-mode opt-in.
func TestChurnWebhook_HMACEnvFields_BothSetIsValid(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvWebhookHMACSecret, "test-secret-32-bytes-or-more-please!!")
	t.Setenv(EnvEnableScaffoldChurnWebhook, "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EnableScaffoldChurnWebhook {
		t.Errorf("EnableScaffoldChurnWebhook: want true, got false")
	}
	if cfg.WebhookHMACSecret != "test-secret-32-bytes-or-more-please!!" {
		t.Errorf("WebhookHMACSecret round-trip mismatch: %q", cfg.WebhookHMACSecret)
	}
}

// TestChurnWebhook_HMACEnvFields_EnableWithoutSecretRejected
// pins the both-or-neither interlock: opting the webhook in
// without a secret is the dangerous footgun (mounted, no auth).
// Load surfaces the misconfig as a Validate error so the process
// fails fast at startup.
func TestChurnWebhook_HMACEnvFields_EnableWithoutSecretRejected(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvEnableScaffoldChurnWebhook, "true")
	// Secret intentionally NOT set.
	_, err := Load()
	if err == nil {
		t.Fatalf("Load: want error for enable=true with empty secret; got nil")
	}
	if !strings.Contains(err.Error(), "CLEAN_CODE_WEBHOOK_HMAC_SECRET") {
		t.Errorf("error should name the missing env var, got %v", err)
	}
}

// TestChurnWebhook_HMACEnvFields_SecretWithoutEnableRejected
// pins the inverse of the previous test: configuring a secret
// but NOT opting the webhook in is also a misconfig (the
// secret will be discarded silently otherwise -- the operator
// probably intended to enable the webhook). Load surfaces this
// as a Validate error.
func TestChurnWebhook_HMACEnvFields_SecretWithoutEnableRejected(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvWebhookHMACSecret, "test-secret-32-bytes-or-more-please!!")
	// Enable flag intentionally NOT set.
	_, err := Load()
	if err == nil {
		t.Fatalf("Load: want error for secret with enable=false; got nil")
	}
	if !strings.Contains(err.Error(), "CLEAN_CODE_ENABLE_SCAFFOLD_CHURN_WEBHOOK") {
		t.Errorf("error should name the missing env var, got %v", err)
	}
}

// TestChurnWebhook_HMACEnvFields_BooleanParsingClosedSet pins
// the closed set of accepted boolean literals for the enable
// flag: standard {true,false,1,0,yes,no,on,off}. Bad literals
// surface as a Load error so an operator typo (e.g. "True ") is
// caught at boot.
func TestChurnWebhook_HMACEnvFields_BooleanParsingClosedSet(t *testing.T) {
	// Strong enough to clear the iter-7 length guard
	// ([MinWebhookHMACSecretBytes]); content is irrelevant
	// for the boolean-parser branches under test.
	strongSecret := "boolean-parser-test-secret-please-be-32-plus-bytes"
	for _, v := range []string{"true", "1", "TRUE", "True"} {
		v := v
		t.Run("accepts/"+v, func(t *testing.T) {
			clearCleanCodeEnv(t)
			t.Setenv(EnvWebhookHMACSecret, strongSecret)
			t.Setenv(EnvEnableScaffoldChurnWebhook, v)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load(%q): %v", v, err)
			}
			if !cfg.EnableScaffoldChurnWebhook {
				t.Errorf("EnableScaffoldChurnWebhook(%q): want true", v)
			}
		})
	}
	for _, v := range []string{"yeah", "absolutely", "2"} {
		v := v
		t.Run("rejects/"+v, func(t *testing.T) {
			clearCleanCodeEnv(t)
			t.Setenv(EnvWebhookHMACSecret, strongSecret)
			t.Setenv(EnvEnableScaffoldChurnWebhook, v)
			if _, err := Load(); err == nil {
				t.Errorf("Load(%q): want error for non-boolean literal; got nil", v)
			}
		})
	}
}

// TestChurnWebhook_HMACEnvFields_MinSecretLengthEnforced pins
// evaluator iter-6 #5: a non-empty secret SHORTER than
// [MinWebhookHMACSecretBytes] must be rejected at startup so
// a one-character typo or copy-paste truncation cannot mount a
// trivially brute-forceable HMAC boundary. Tests at exactly
// (min-1) and (min) the boundary.
func TestChurnWebhook_HMACEnvFields_MinSecretLengthEnforced(t *testing.T) {
	// Just-under-minimum -> reject.
	t.Run("rejects-31-bytes", func(t *testing.T) {
		clearCleanCodeEnv(t)
		shortSecret := strings.Repeat("x", MinWebhookHMACSecretBytes-1)
		t.Setenv(EnvWebhookHMACSecret, shortSecret)
		t.Setenv(EnvEnableScaffoldChurnWebhook, "true")
		_, err := Load()
		if err == nil {
			t.Fatalf("Load: want error for %d-byte secret; got nil", len(shortSecret))
		}
		if !strings.Contains(err.Error(), "at least") {
			t.Errorf("error should explain the minimum-length guard, got %v", err)
		}
		if !strings.Contains(err.Error(), EnvWebhookHMACSecret) {
			t.Errorf("error should name %s, got %v", EnvWebhookHMACSecret, err)
		}
	})
	// Exactly minimum -> accept.
	t.Run("accepts-32-bytes", func(t *testing.T) {
		clearCleanCodeEnv(t)
		exactSecret := strings.Repeat("x", MinWebhookHMACSecretBytes)
		t.Setenv(EnvWebhookHMACSecret, exactSecret)
		t.Setenv(EnvEnableScaffoldChurnWebhook, "true")
		cfg, err := Load()
		if err != nil {
			t.Fatalf("Load: want no error for %d-byte secret; got %v", len(exactSecret), err)
		}
		if cfg.WebhookHMACSecret != exactSecret {
			t.Errorf("WebhookHMACSecret round-trip mismatch")
		}
	})
	// Empty -> still accepted (means "webhook disabled"); the
	// both-or-neither interlock takes precedence over the
	// length guard. This is the default production state.
	t.Run("accepts-empty-when-enable-flag-false", func(t *testing.T) {
		clearCleanCodeEnv(t)
		// Both unset -> webhook off, no length-guard hit.
		if _, err := Load(); err != nil {
			t.Errorf("Load with both env vars unset: want nil; got %v", err)
		}
	})
}
