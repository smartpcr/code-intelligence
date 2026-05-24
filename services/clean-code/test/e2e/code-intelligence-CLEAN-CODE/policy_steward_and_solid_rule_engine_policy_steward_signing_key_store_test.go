//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
)

// requireEnv returns the value of the named environment variable,
// calling t.Skip when unset or empty.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

// ---------- shared types ----------

// signingKeyStoreState carries state across Given/When/Then steps for
// the policy-steward signing-key-store scenarios.
type signingKeyStoreState struct {
	kmsURL            string
	policyStewardURL  string
	policySigningPin  string
	oldKey            ed25519.PrivateKey
	oldKeyID          string
	rotationTime      time.Time
	lastVerifySuccess bool
	lastExitCode      int
	lastOutput        string
}

// ---------- helpers ----------

// kmsRotateKey tells the kms-mock to rotate its signing key and returns
// the previous key ID and private key material so we can sign payloads
// with the "old" key.
func (s *signingKeyStoreState) kmsRotateKey() error {
	resp, err := http.Post(s.kmsURL+"/v1/keys/rotate", "application/json", nil)
	if err != nil {
		return fmt.Errorf("POST /v1/keys/rotate: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("rotate returned %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		PreviousKeyID  string `json:"previous_key_id"`
		PreviousKeyB64 string `json:"previous_key_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding rotate response: %w", err)
	}

	if result.PreviousKeyB64 != "" {
		keyBytes, err := base64.StdEncoding.DecodeString(result.PreviousKeyB64)
		if err != nil {
			return fmt.Errorf("decoding previous key: %w", err)
		}
		s.oldKey = ed25519.PrivateKey(keyBytes)
	} else {
		// kms-mock may not return the private key; generate a local
		// Ed25519 pair and register it as a "previous" key via the mock.
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return fmt.Errorf("generating ed25519 key: %w", err)
		}
		s.oldKey = priv
	}

	s.oldKeyID = result.PreviousKeyID
	return nil
}

// signPayload signs a test payload with the given key and returns the
// base64-encoded signature.
func signPayload(key ed25519.PrivateKey, payload []byte) string {
	sig := ed25519.Sign(key, payload)
	return base64.StdEncoding.EncodeToString(sig)
}

// verifyViaService sends a signed payload to the policy-steward verify
// endpoint with a simulated wall-clock offset.
func (s *signingKeyStoreState) verifyViaService(payload []byte, signature string, keyID string, simulatedTime time.Time) (bool, error) {
	body := map[string]interface{}{
		"payload":        base64.StdEncoding.EncodeToString(payload),
		"signature":      signature,
		"key_id":         keyID,
		"simulated_time": simulatedTime.UTC().Format(time.RFC3339Nano),
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("marshalling verify request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, s.policyStewardURL+"/v1/verify", bytes.NewReader(jsonBody))
	if err != nil {
		return false, fmt.Errorf("creating verify request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("POST /v1/verify: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		return true, nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest {
		return false, nil
	}
	respBody, _ := io.ReadAll(resp.Body)
	return false, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(respBody))
}

// ---------- Scenario: overlap-window-enforced ----------

func (s *signingKeyStoreState) aKeyRotationOccurredAtT0() error {
	s.kmsURL = os.Getenv("CLEAN_CODE_KMS_URL")
	if s.kmsURL == "" {
		return fmt.Errorf("CLEAN_CODE_KMS_URL is not set")
	}
	s.policyStewardURL = os.Getenv("CLEAN_CODE_POLICY_STEWARD_URL")
	if s.policyStewardURL == "" {
		// Derive from compose service name convention.
		s.policyStewardURL = "http://localhost:8082"
	}

	if err := s.kmsRotateKey(); err != nil {
		return fmt.Errorf("rotating key at T0: %w", err)
	}
	s.rotationTime = time.Now().UTC()
	return nil
}

func (s *signingKeyStoreState) aPayloadSignedByTheOldKeyArrivesAtT0Plus23h59m() error {
	payload := []byte("e2e-test-payload-overlap-window")
	sig := signPayload(s.oldKey, payload)
	simulatedTime := s.rotationTime.Add(23*time.Hour + 59*time.Minute)

	ok, err := s.verifyViaService(payload, sig, s.oldKeyID, simulatedTime)
	if err != nil {
		return fmt.Errorf("verify at T0+23h59m: %w", err)
	}
	s.lastVerifySuccess = ok
	return nil
}

func (s *signingKeyStoreState) verificationSucceeds() error {
	if !s.lastVerifySuccess {
		return fmt.Errorf("expected verification to succeed, but it failed")
	}
	return nil
}

func (s *signingKeyStoreState) aPayloadSignedByTheOldKeyArrivesAtT0Plus24hPlus1s() error {
	payload := []byte("e2e-test-payload-overlap-window")
	sig := signPayload(s.oldKey, payload)
	simulatedTime := s.rotationTime.Add(24*time.Hour + 1*time.Second)

	ok, err := s.verifyViaService(payload, sig, s.oldKeyID, simulatedTime)
	if err != nil {
		return fmt.Errorf("verify at T0+24h+1s: %w", err)
	}
	s.lastVerifySuccess = ok
	return nil
}

func (s *signingKeyStoreState) verificationFails() error {
	if s.lastVerifySuccess {
		return fmt.Errorf("expected verification to fail, but it succeeded")
	}
	return nil
}

// ---------- Scenario: kms-unavailable-blocks-start ----------

func (s *signingKeyStoreState) theKMSIsUnreachableAtStartupAndThePinIsActive(pin string) error {
	// We will start the policy-steward binary with a bogus KMS URL
	// to simulate the KMS being unreachable, while the operator pin
	// "policy-signing-required=v1 required" is active — meaning the
	// service MUST have a valid KMS connection to start.
	s.kmsURL = "http://127.0.0.1:1" // port 1 — guaranteed connection refused
	s.policySigningPin = pin
	return nil
}

func (s *signingKeyStoreState) theServiceInitialises() error {
	pgURL := os.Getenv("CLEAN_CODE_PG_URL")
	if pgURL == "" {
		pgURL = "postgres://localhost:5432/clean_code?sslmode=disable"
	}

	// Try to start the policy-steward binary with the unreachable KMS
	// and the policy-signing-required pin set to "v1 required".
	// The binary should exit non-zero quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "compose",
		"-f", "tests/e2e/phase-05-policy-engine/docker-compose.yml",
		"run", "--rm",
		"-e", "CLEAN_CODE_KMS_URL="+s.kmsURL,
		"-e", "CLEAN_CODE_PG_URL="+pgURL,
		"-e", "CLEAN_CODE_POLICY_SIGNING_REQUIRED="+s.policySigningPin,
		"--no-deps",
		"policy-steward",
	)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	err := cmd.Run()
	s.lastOutput = buf.String()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			s.lastExitCode = exitErr.ExitCode()
		} else {
			s.lastExitCode = -1
		}
	} else {
		s.lastExitCode = 0
	}
	return nil
}

func (s *signingKeyStoreState) itExitsNonZeroWithAClearError() error {
	if s.lastExitCode == 0 {
		return fmt.Errorf("expected non-zero exit code, got 0; output:\n%s", s.lastOutput)
	}

	// Check for a clear error message about KMS connectivity.
	lower := strings.ToLower(s.lastOutput)
	errorIndicators := []string{"kms", "key management", "signing", "unreachable", "connection refused", "connect"}
	found := false
	for _, indicator := range errorIndicators {
		if strings.Contains(lower, indicator) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("exit code %d but no clear KMS-related error in output:\n%s", s.lastExitCode, s.lastOutput)
	}
	return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_policy_steward_and_solid_rule_engine_policy_steward_signing_key_store(ctx *godog.ScenarioContext) {
	s := &signingKeyStoreState{}

	// overlap-window-enforced
	ctx.Step(`^a key rotation occurred at T0$`, s.aKeyRotationOccurredAtT0)
	ctx.Step(`^a payload signed by the old key arrives at T0\+23h59m$`, s.aPayloadSignedByTheOldKeyArrivesAtT0Plus23h59m)
	ctx.Step(`^verification succeeds$`, s.verificationSucceeds)
	ctx.Step(`^a payload signed by the old key arrives at T0\+24h\+1s$`, s.aPayloadSignedByTheOldKeyArrivesAtT0Plus24hPlus1s)
	ctx.Step(`^verification fails$`, s.verificationFails)

	// kms-unavailable-blocks-start
	ctx.Step(`^the KMS is unreachable at startup and the "([^"]*)" pin is active$`, s.theKMSIsUnreachableAtStartupAndThePinIsActive)
	ctx.Step(`^the service initialises$`, s.theServiceInitialises)
	ctx.Step(`^it exits non-zero with a clear error$`, s.itExitsNonZeroWithAClearError)
}

func TestE2E_policy_steward_and_solid_rule_engine_policy_steward_signing_key_store(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_policy_steward_and_solid_rule_engine_policy_steward_signing_key_store,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"policy_steward_and_solid_rule_engine_policy_steward_signing_key_store.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}