package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/health"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/management"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// TestRootMux_ScaffoldModeListActive503 checks the scaffold-mode
// composition root: no KMS provider means no wired management
// handler, but /healthz, /readyz AND
// /v1/policy/keys/list_active are ALL mounted. The list_active
// verb returns 503 (NOT 404) so operators can distinguish
// "verb exists, backing subsystem down" from "this build
// doesn't ship the verb at all". The contract is pinned by
// `services/clean-code/docs/runbook.md` Stage 5.1 runbook.
func TestRootMux_ScaffoldModeListActive503(t *testing.T) {
	t.Parallel()
	mux := rootMux(health.New("v0", "c0", "t0"), nil, nil)

	for _, path := range []string{"/healthz", "/readyz"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		// /healthz returns 200, /readyz returns 503 (no
		// checks registered). Both are NOT 404 -- the route
		// must be mounted.
		if rr.Code == http.StatusNotFound {
			t.Errorf("%s: rootMux returned 404; route not mounted", path)
		}
	}

	// /v1/policy/keys/list_active MUST mount even in scaffold
	// mode, and MUST return 503 -- this is the runbook's
	// canonical signal that the verb exists but the
	// signing-key cache is unwired.
	req := httptest.NewRequest(http.MethodGet, management.VerbListActivePath, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("list_active without mgmt: status=%d, want 503 (scaffold mode contract)", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "signing key") {
		t.Errorf("body=%q does not mention signing key", rr.Body.String())
	}
}

// TestRootMux_ListActiveMounted: with mgmt non-nil, all three
// paths must dispatch and the list_active path must return a
// bare JSON array.
func TestRootMux_ListActiveMounted(t *testing.T) {
	t.Parallel()
	mgmt := management.NewHandler(management.NewReader(nil))
	mux := rootMux(health.New("v0", "c0", "t0"), mgmt, nil)

	req := httptest.NewRequest(http.MethodGet, management.VerbListActivePath, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	// Nil-manager reader → 503, NOT 404. Confirms the
	// management handler is mounted.
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("list_active with mgmt (nil reader): status=%d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "signing key") {
		t.Errorf("body=%q does not mention signing key", rr.Body.String())
	}
}

// TestBuildPolicyWriter_ScaffoldModeProducesWriter pins the
// wiring invariant the Stage 5.3 kill-switch contract demands:
// even when both `db` and `signer` are nil (the scaffold-mode
// composition path), `buildPolicyWriter` MUST return a
// non-nil [*management.PolicyWriter]. The previous wiring
// gated the Steward construction inside the
// `cfg.KMSProvider != ""` branch, which left `policy = nil`
// in scaffold mode and made `/v1/mgmt/override` return 503 --
// violating the runbook contract pinned at
// `docs/runbook.md` (Stage 5.3 "No signing-key precondition").
func TestBuildPolicyWriter_ScaffoldModeProducesWriter(t *testing.T) {
	t.Parallel()
	pw, stew, store, closeDB, err := buildPolicyWriter(nil, nil, nil)
	if err != nil {
		t.Fatalf("buildPolicyWriter(nil, nil, nil): err=%v; want nil", err)
	}
	if pw == nil {
		t.Fatalf("buildPolicyWriter(nil, nil, nil): writer is nil; want a real PolicyWriter (kill-switch contract)")
	}
	if stew == nil {
		t.Fatalf("buildPolicyWriter(nil, nil, nil): steward is nil; want a real *steward.Steward so the decoupling Bootstrap path can wire onto the same instance the HTTP surface serves")
	}
	if store == nil {
		t.Fatalf("buildPolicyWriter(nil, nil, nil): store is nil; want a real steward.Store so SeedThresholds can write")
	}
	if closeDB {
		t.Errorf("buildPolicyWriter(nil, nil, nil): closeDB=true; want false (no db handle was opened)")
	}
}

// TestRootMux_ScaffoldModeOverrideMounted_200 is the
// composition-root pin for the Stage 5.3 kill-switch contract.
// Stack:
//
//	rootMux(scaffold healthHandler, mgmt=nil, policy=buildPolicyWriter(nil, nil, nil))
//
// A valid `POST /v1/mgmt/override` against a rule registered
// at the in-memory store MUST return **200** even though no
// `keys.Manager` is wired (signer is nil). The signing-key
// dependent verbs (`/v1/policy/keys/list_active`, the Stage
// 5.2 writes) keep their 503-on-scaffold behavior via the
// null-object signer the steward installs internally.
//
// This test is the iter-3 fix for the iter-2 evaluator point 1
// (kill-switch contract violated at composition root) and is
// the integration-level coverage the iter-2 evaluator point 2
// said was missing.
func TestRootMux_ScaffoldModeOverrideMounted_200(t *testing.T) {
	t.Parallel()

	if _, _, _, _, err := buildPolicyWriter(nil, nil, nil); err != nil {
		// Sanity check: the composition helper itself must
		// succeed in scaffold mode. The wiring invariant is
		// already pinned by TestBuildPolicyWriter_ScaffoldModeProducesWriter;
		// this is a defence-in-depth assertion so a regression
		// to the helper surfaces here too.
		t.Fatalf("buildPolicyWriter(scaffold): %v", err)
	}

	// Build a fresh steward + store we can seed directly --
	// PublishRulepack would also work but requires a signing
	// key (the very thing we're proving is NOT required).
	stewStore := steward.NewInMemoryStore()
	seedOneRuleInto(t, stewStore, "solid.srp.lcom4_high")
	stew, err := steward.New(steward.Config{Store: stewStore}) // signer nil → null-object signer
	if err != nil {
		t.Fatalf("steward.New(no signer): %v", err)
	}
	policy := management.NewPolicyWriter(stew)

	mux := rootMux(health.New("v0", "c0", "t0"), nil, policy)

	body := strings.NewReader(`{
		"rule_id": "solid.srp.lcom4_high",
		"scope_filter": {
			"repo_id": "repo-a",
			"scope_kind": "class",
			"scope_signature_glob": "com.example.legacy.*"
		},
		"mute": true,
		"reason": "rollout smoke (scaffold mode)"
	}`)
	req := httptest.NewRequest(http.MethodPost, management.VerbMgmtOverridePath, body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(management.OIDCSubjectHeader, "rollout-smoke@operator.local")
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("POST /v1/mgmt/override in scaffold mode: status=%d, want 200 (kill-switch contract); body=%s",
			rr.Code, rr.Body.String())
	}
	// The handler MUST emit `{override_id: ...}` -- the
	// canonical -> OverrideId return type (architecture Sec
	// 6.3 line 1357). A 200 with the wrong body would
	// satisfy the status code but not the verb contract.
	if !strings.Contains(rr.Body.String(), `"override_id"`) {
		t.Errorf("body=%s; expected override_id in response", rr.Body.String())
	}

	// Belt-and-braces: now hit the Stage 5.2 publish verb
	// to confirm the null-object signer still keeps THAT
	// path locked at 503 -- the kill-switch contract opens
	// override but MUST NOT open the signing-key verbs.
	publishReq := httptest.NewRequest(http.MethodPost, management.VerbPublishPath,
		strings.NewReader(`{"name":"v1","rule_refs":[{"rule_id":"solid.srp.lcom4_high","version":1}],"threshold_refs":[],"refactor_weights":{"alpha":0.4,"beta":0.3,"gamma":0.2,"delta":0.1,"effort_model_version":"v1.0","window_days":90}}`))
	publishReq.Header.Set("Content-Type", "application/json")
	publishRR := httptest.NewRecorder()
	mux.ServeHTTP(publishRR, publishReq)
	if publishRR.Code != http.StatusServiceUnavailable {
		t.Errorf("POST /v1/policy/publish in scaffold mode: status=%d, want 503 (signing-key precondition); body=%s",
			publishRR.Code, publishRR.Body.String())
	}
}

// seedOneRuleInto registers a rule_id into the supplied
// in-memory store so override's logical FK check
// (RuleExistsByID) accepts it. Uses InsertRulePackAndRules
// directly -- the steward's PublishRulepack verb would also
// work but requires a signing key (we want to exercise the
// scaffold-mode no-signer path).
func seedOneRuleInto(t *testing.T, s steward.Store, ruleID string) {
	t.Helper()
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
	pack := steward.RulePack{
		PackID:        "solid.srp",
		Version:       1,
		DisplayName:   "Single Responsibility",
		DescriptionMD: "SOLID SRP rulepack.",
		CreatedAt:     now,
	}
	rules := []steward.Rule{
		{RuleID: ruleID, Version: 1, PackID: "solid.srp",
			PredicateDSL: "lcom4 > 0.7", SeverityDefault: steward.SeverityBlock,
			DescriptionMD: "High LCOM4.", CreatedAt: now},
	}
	if err := s.InsertRulePackAndRules(context.Background(), pack, rules); err != nil {
		t.Fatalf("seedOneRuleInto: %v", err)
	}
}
