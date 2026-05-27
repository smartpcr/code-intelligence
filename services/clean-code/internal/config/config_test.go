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
		EnvMgmtPGURL,
		EnvAllowSharedPGRole,
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
		EnvEnableScaffoldIndexerWebhook,
		EnvEnableExternalIngestWebhook,
		EnvWebhookSigningKeyID,
		EnvDisableStaleSweep,
		EnvEnableLegacyDemoAPI,
		EnvAstScanRoot,
		EnvAggregatorCadence,
		EnvDisableAggregator,
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
	// Stage 7.1 knobs: tech-spec Sec 8.2 default
	// `aggregator_cadence=15m`; aggregator loop enabled by
	// default.
	if cfg.AggregatorCadence != 15*time.Minute {
		t.Errorf("AggregatorCadence = %s; want 15m", cfg.AggregatorCadence)
	}
	if cfg.AggregatorCadence != DefaultAggregatorCadence {
		t.Errorf("AggregatorCadence = %s; want DefaultAggregatorCadence (%s)", cfg.AggregatorCadence, DefaultAggregatorCadence)
	}
	if cfg.DisableAggregator {
		t.Errorf("DisableAggregator = true; want false (loop enabled by default)")
	}
}

// TestLoad_AggregatorEnvOverrides verifies the Stage 7.1
// `CLEAN_CODE_AGGREGATOR_CADENCE` + `CLEAN_CODE_DISABLE_AGGREGATOR`
// knobs round-trip through Load and override the
// `DefaultAggregatorCadence` / disabled=false defaults.
func TestLoad_AggregatorEnvOverrides(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvAggregatorCadence, "7m30s")
	t.Setenv(EnvDisableAggregator, "true")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.AggregatorCadence != 7*time.Minute+30*time.Second {
		t.Errorf("AggregatorCadence = %s; want 7m30s", cfg.AggregatorCadence)
	}
	if !cfg.DisableAggregator {
		t.Errorf("DisableAggregator = false; want true")
	}
}

// TestLoad_AggregatorCadenceMalformedRejected verifies a malformed
// cadence value surfaces an error that names the env key (so the
// operator can identify which knob to fix). The fail-fast contract
// is the same as every other duration-typed knob.
func TestLoad_AggregatorCadenceMalformedRejected(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvAggregatorCadence, "fifteen-minutes")
	if _, err := Load(); err == nil {
		t.Errorf("Load: want non-nil error for bogus AggregatorCadence, got nil")
	} else if !strings.Contains(err.Error(), EnvAggregatorCadence) {
		t.Errorf("Load: error %q must name %s for operator triage", err, EnvAggregatorCadence)
	}
}

// TestLoad_AggregatorCadenceNonPositiveRejected verifies the
// `> 0` validation in Config.Validate -- a zero or negative
// cadence would loop tightly and is rejected at load time.
func TestLoad_AggregatorCadenceNonPositiveRejected(t *testing.T) {
	for _, raw := range []string{"0s", "-5m"} {
		t.Run(raw, func(t *testing.T) {
			clearCleanCodeEnv(t)
			t.Setenv(EnvAggregatorCadence, raw)
			if _, err := Load(); err == nil {
				t.Errorf("Load: want non-nil error for AggregatorCadence=%s, got nil", raw)
			}
		})
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

// --- Stage 3.5 iter 3: stale-sweep + legacy-demo env round-trip ---

// TestStaleSweep_EnvFields_DefaultsAreEnabled pins the
// production-default semantics for iter-3 evaluator item 2: when
// no operator env var is set, the sweep is enabled (DisableStaleSweep
// == false) and the legacy demo API is unmounted
// (EnableLegacyDemoAPI == false). Both defaults match
// architecture's "canonical surface only, sweep on" stance.
func TestStaleSweep_EnvFields_DefaultsAreEnabled(t *testing.T) {
	clearCleanCodeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DisableStaleSweep {
		t.Errorf("DisableStaleSweep: want false (sweep enabled by default), got true")
	}
	if cfg.EnableLegacyDemoAPI {
		t.Errorf("EnableLegacyDemoAPI: want false (legacy demo unmounted by default), got true")
	}
}

// TestStaleSweep_EnvFields_RoundTripBooleans verifies the
// boolean opt-out / opt-in literals (1|true|yes|on /
// 0|false|no|off) round-trip through Load. The iter-3 evaluator
// flagged that the previous direct os.Getenv call in main.go
// bypassed this contract; we assert it here so a future
// regression breaks the test.
func TestStaleSweep_EnvFields_RoundTripBooleans(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"1", true}, {"true", true}, {"yes", true}, {"on", true},
		{"0", false}, {"false", false}, {"no", false}, {"off", false},
	}
	for _, c := range cases {
		t.Run("DisableStaleSweep="+c.raw, func(t *testing.T) {
			clearCleanCodeEnv(t)
			t.Setenv(EnvDisableStaleSweep, c.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DisableStaleSweep != c.want {
				t.Errorf("DisableStaleSweep(%q): got %v, want %v", c.raw, cfg.DisableStaleSweep, c.want)
			}
		})
		t.Run("EnableLegacyDemoAPI="+c.raw, func(t *testing.T) {
			clearCleanCodeEnv(t)
			t.Setenv(EnvEnableLegacyDemoAPI, c.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.EnableLegacyDemoAPI != c.want {
				t.Errorf("EnableLegacyDemoAPI(%q): got %v, want %v", c.raw, cfg.EnableLegacyDemoAPI, c.want)
			}
		})
	}
}

// TestStaleSweep_EnvFields_RejectsNonBoolean pins the fail-fast
// contract: a malformed value MUST produce a non-nil Load error
// so an operator typo cannot silently flip back to the default.
func TestStaleSweep_EnvFields_RejectsNonBoolean(t *testing.T) {
	for _, env := range []string{EnvDisableStaleSweep, EnvEnableLegacyDemoAPI} {
		t.Run(env+"=bogus", func(t *testing.T) {
			clearCleanCodeEnv(t)
			t.Setenv(env, "bogus")
			if _, err := Load(); err == nil {
				t.Errorf("Load with %s=bogus: want non-nil error, got nil", env)
			}
		})
	}
}

// TestMgmtPGURL_DefaultsAreEmpty pins iter-3 evaluator item #1
// production-default semantics: when no operator env var is set,
// the management-role DSN is empty and the shared-role opt-in is
// false -- production main() will then refuse to mount the mgmt.*
// write verbs and fail fast at startup.
func TestMgmtPGURL_DefaultsAreEmpty(t *testing.T) {
	clearCleanCodeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ManagementPostgresURL != "" {
		t.Errorf("ManagementPostgresURL default: got %q, want empty (operators MUST set CLEAN_CODE_MGMT_PG_URL)", cfg.ManagementPostgresURL)
	}
	if cfg.AllowSharedPGRole {
		t.Errorf("AllowSharedPGRole default: got true, want false (shared-role mode is opt-in)")
	}
}

// TestMgmtPGURL_RoundTripsThroughLoad pins the env var → Config
// field plumbing for the new Stage 3.4 iter-3 management-role
// fields. Without this assertion a future refactor of
// applyOverrides could silently strip the case branch.
func TestMgmtPGURL_RoundTripsThroughLoad(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvMgmtPGURL, "postgres://mgmt-role@host:5432/db")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.ManagementPostgresURL, "postgres://mgmt-role@host:5432/db"; got != want {
		t.Errorf("ManagementPostgresURL: got %q, want %q", got, want)
	}
}

// TestAllowSharedPGRole_RoundTripsBooleans verifies the boolean
// literal parsing for the dev/E2E opt-in flag mirrors the other
// CLEAN_CODE_*_ENABLE_* flags.
func TestAllowSharedPGRole_RoundTripsBooleans(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"1", true}, {"true", true}, {"yes", true}, {"on", true},
		{"0", false}, {"false", false}, {"no", false}, {"off", false},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			clearCleanCodeEnv(t)
			t.Setenv(EnvAllowSharedPGRole, c.raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.AllowSharedPGRole != c.want {
				t.Errorf("AllowSharedPGRole(%q): got %v, want %v", c.raw, cfg.AllowSharedPGRole, c.want)
			}
		})
	}
}

// TestAllowSharedPGRole_RejectsNonBoolean pins fail-fast on a
// malformed value so an operator typo cannot silently disable the
// role-distinct fail-fast guard.
func TestAllowSharedPGRole_RejectsNonBoolean(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvAllowSharedPGRole, "maybe")
	if _, err := Load(); err == nil {
		t.Errorf("Load with %s=maybe: want non-nil error, got nil", EnvAllowSharedPGRole)
	}
}

// strongHMACSecret returns an HMAC secret long enough to clear
// [MinWebhookHMACSecretBytes] -- a constant test fixture used
// by the external-ingest interlock tests below.
func strongHMACSecret() string {
	return strings.Repeat("z", MinWebhookHMACSecretBytes)
}

// TestExternalIngestWebhook_AllThreeVarsSet_AcceptsAndRoundTrips
// (iter-3 evaluator item #3) pins the happy path of the
// three-variable interlock for the external-ingest Router:
// when [EnvEnableExternalIngestWebhook] AND
// [EnvWebhookHMACSecret] AND [EnvWebhookSigningKeyID] are
// all set, Load succeeds and the values round-trip onto the
// Config.
func TestExternalIngestWebhook_AllThreeVarsSet_AcceptsAndRoundTrips(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvEnableExternalIngestWebhook, "1")
	t.Setenv(EnvWebhookHMACSecret, strongHMACSecret())
	t.Setenv(EnvWebhookSigningKeyID, "key-prod-01")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.EnableExternalIngestWebhook {
		t.Errorf("EnableExternalIngestWebhook: want true, got false")
	}
	if cfg.WebhookSigningKeyID != "key-prod-01" {
		t.Errorf("WebhookSigningKeyID: want %q, got %q", "key-prod-01", cfg.WebhookSigningKeyID)
	}
	if cfg.WebhookHMACSecret != strongHMACSecret() {
		t.Errorf("WebhookHMACSecret round-trip mismatch")
	}
}

// TestExternalIngestWebhook_EnableWithoutHMACSecret_Rejected
// pins the first half of the interlock: enabling the
// webhook without supplying the HMAC secret is a deployment
// misconfiguration that MUST fail loudly at Load.
func TestExternalIngestWebhook_EnableWithoutHMACSecret_Rejected(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvEnableExternalIngestWebhook, "1")
	t.Setenv(EnvWebhookSigningKeyID, "key-prod-01")
	// EnvWebhookHMACSecret deliberately left unset.
	_, err := Load()
	if err == nil {
		t.Fatalf("Load: want error (missing %s), got nil", EnvWebhookHMACSecret)
	}
	if !strings.Contains(err.Error(), EnvWebhookHMACSecret) {
		t.Errorf("error must name %s for operator triage, got: %v", EnvWebhookHMACSecret, err)
	}
	if !strings.Contains(err.Error(), EnvEnableExternalIngestWebhook) {
		t.Errorf("error must name %s, got: %v", EnvEnableExternalIngestWebhook, err)
	}
}

// TestExternalIngestWebhook_EnableWithoutSigningKeyID_Rejected
// pins the second half of the interlock: enabling the
// webhook without the signing_key_id leaves the secret
// resolver unable to verify any signature, so Load MUST
// reject.
func TestExternalIngestWebhook_EnableWithoutSigningKeyID_Rejected(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvEnableExternalIngestWebhook, "1")
	t.Setenv(EnvWebhookHMACSecret, strongHMACSecret())
	// EnvWebhookSigningKeyID deliberately left unset.
	_, err := Load()
	if err == nil {
		t.Fatalf("Load: want error (missing %s), got nil", EnvWebhookSigningKeyID)
	}
	if !strings.Contains(err.Error(), EnvWebhookSigningKeyID) {
		t.Errorf("error must name %s, got: %v", EnvWebhookSigningKeyID, err)
	}
}

// TestExternalIngestWebhook_SigningKeyIDWithoutEnable_Rejected
// pins the inverse direction of the interlock: setting the
// signing_key_id without the explicit enable flag is a
// fingerprint of a half-finished rollout (operator forgot
// the enable). Load MUST reject so the misconfiguration
// surfaces before any HTTP traffic arrives.
func TestExternalIngestWebhook_SigningKeyIDWithoutEnable_Rejected(t *testing.T) {
	clearCleanCodeEnv(t)
	t.Setenv(EnvWebhookSigningKeyID, "key-prod-01")
	t.Setenv(EnvWebhookHMACSecret, strongHMACSecret())
	// EnvEnableExternalIngestWebhook deliberately left unset.
	_, err := Load()
	if err == nil {
		t.Fatalf("Load: want error (signing_key_id without enable), got nil")
	}
	if !strings.Contains(err.Error(), EnvEnableExternalIngestWebhook) {
		t.Errorf("error must name %s, got: %v", EnvEnableExternalIngestWebhook, err)
	}
}

// TestExternalIngestWebhook_UnsetByDefault pins the
// off-by-default posture: with NO webhook env vars set, the
// external-ingest flag is false and the signing_key_id is
// empty. Required so a service binary that doesn't opt in
// will never accidentally accept external webhook traffic.
func TestExternalIngestWebhook_UnsetByDefault(t *testing.T) {
	clearCleanCodeEnv(t)
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.EnableExternalIngestWebhook {
		t.Errorf("EnableExternalIngestWebhook: want false (off-by-default), got true")
	}
	if cfg.WebhookSigningKeyID != "" {
		t.Errorf("WebhookSigningKeyID: want empty (off-by-default), got %q", cfg.WebhookSigningKeyID)
	}
}

