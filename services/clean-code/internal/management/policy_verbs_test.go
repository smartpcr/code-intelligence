package management

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// buildStewardWithMintedKey returns a real
// `*steward.Steward` wired to an InMemoryStore and a one-key
// `*keys.Manager`. Mirrors the production composition root.
//
// The returned steward's store is PRE-SEEDED with the rule
// referenced by [samplePublishBody] (rule_id=solid.srp.lcom4_high,
// version=1) so happy-path Publish tests succeed; this mirrors
// the operator flow where `policy.publish_rulepack` runs
// before the first `policy.publish` call. Tests that need an
// unseeded store (e.g. the FK-enforcement negative case) use
// [buildStewardWithEmptyStore] instead.
func buildStewardWithMintedKey(t *testing.T) *steward.Steward {
	t.Helper()
	st, _ := buildStewardWithEmptyStore(t)
	if _, _, err := st.PublishRulepack(context.Background(), steward.PublishRulepackRequest{
		PackID:        "solid.srp",
		Version:       1,
		DisplayName:   "Single Responsibility",
		DescriptionMD: "SOLID SRP rulepack.",
		Rules: []steward.RuleSpec{
			{RuleID: "solid.srp.lcom4_high", Version: 1,
				PredicateDSL: "lcom4 > 0.7", SeverityDefault: steward.SeverityBlock,
				DescriptionMD: "High LCOM4."},
		},
	}); err != nil {
		t.Fatalf("seed sample rulepack: %v", err)
	}
	return st
}

// buildStewardWithEmptyStore returns a steward backed by an
// empty in-memory store and a minted signing key. Used by FK-
// enforcement negative tests that need to assert behaviour when
// a rule/threshold is NOT registered. Also returns the
// underlying store so the test can seed rows directly if it
// chooses.
func buildStewardWithEmptyStore(t *testing.T) (*steward.Steward, *steward.InMemoryStore) {
	t.Helper()
	res, err := keys.Build(context.Background(), keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	t.Cleanup(res.Close)
	store := steward.NewInMemoryStore()
	st, err := steward.New(steward.Config{
		Store:  store,
		Signer: res.Manager,
	})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	return st, store
}

// samplePublishBody mints a JSON wire body the Publish handler
// accepts as a happy-path payload.
func samplePublishBody(t *testing.T) []byte {
	t.Helper()
	body := map[string]any{
		"name": "default-v1",
		"rule_refs": []map[string]any{
			{"rule_id": "solid.srp.lcom4_high", "version": 1},
		},
		"threshold_refs": []map[string]any{},
		"refactor_weights": map[string]any{
			"alpha": 0.4, "beta": 0.3, "gamma": 0.2, "delta": 0.1,
			"effort_model_version": "v1.0",
			"window_days":          90,
		},
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal sample body: %v", err)
	}
	return out
}

func samplePublishRulepackBody(t *testing.T) []byte {
	t.Helper()
	body := map[string]any{
		"pack_id":        "solid.srp",
		"version":        1,
		"display_name":   "Single Responsibility",
		"description_md": "SOLID SRP rulepack.",
		"rules": []map[string]any{
			{
				"rule_id":          "solid.srp.lcom4_high",
				"version":          1,
				"predicate_dsl":    "lcom4 > 0.7",
				"severity_default": "block",
				"description_md":   "High LCOM4.",
			},
		},
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal sample body: %v", err)
	}
	return out
}

// ---- happy paths ------------------------------------------

func TestPolicyWriter_Publish_HappyPath(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	req := httptest.NewRequest(http.MethodPost, VerbPublishPath, bytes.NewReader(samplePublishBody(t)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	pw.Publish(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var pv steward.PolicyVersion
	if err := json.Unmarshal(rr.Body.Bytes(), &pv); err != nil {
		t.Fatalf("response not a PolicyVersion: %v; body=%s", err, rr.Body.String())
	}
	if pv.PolicyVersionID == uuid.Nil {
		t.Errorf("response.policy_version_id is the zero uuid")
	}
	if len(pv.Signature) == 0 {
		t.Errorf("response.signature is empty")
	}
}

func TestPolicyWriter_Activate_HappyPath(t *testing.T) {
	t.Parallel()
	st := buildStewardWithMintedKey(t)
	pw := NewPolicyWriter(st)

	// First publish a row so the activate has a real
	// policy_version_id to target.
	pubReq := httptest.NewRequest(http.MethodPost, VerbPublishPath, bytes.NewReader(samplePublishBody(t)))
	pubRR := httptest.NewRecorder()
	pw.Publish(pubRR, pubReq)
	if pubRR.Code != http.StatusOK {
		t.Fatalf("setup Publish: status=%d, want 200", pubRR.Code)
	}
	var pv steward.PolicyVersion
	if err := json.Unmarshal(pubRR.Body.Bytes(), &pv); err != nil {
		t.Fatalf("setup unmarshal: %v", err)
	}

	body, err := json.Marshal(map[string]string{
		"policy_version_id": pv.PolicyVersionID.String(),
		"activated_by":      "alice",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, VerbActivatePath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	pw.Activate(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var pa steward.PolicyActivation
	if err := json.Unmarshal(rr.Body.Bytes(), &pa); err != nil {
		t.Fatalf("response not a PolicyActivation: %v", err)
	}
	if pa.PolicyVersionID != pv.PolicyVersionID {
		t.Errorf("response.policy_version_id=%s, want %s", pa.PolicyVersionID, pv.PolicyVersionID)
	}
}

func TestPolicyWriter_PublishRulepack_HappyPath(t *testing.T) {
	t.Parallel()
	st, _ := buildStewardWithEmptyStore(t)
	pw := NewPolicyWriter(st)
	req := httptest.NewRequest(http.MethodPost, VerbPublishRulepackPath, bytes.NewReader(samplePublishRulepackBody(t)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	pw.PublishRulepack(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp publishRulepackResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.RulePack.PackID != "solid.srp" {
		t.Errorf("response.rule_pack.pack_id=%q, want %q", resp.RulePack.PackID, "solid.srp")
	}
	if len(resp.Rules) != 1 {
		t.Errorf("response.rules count=%d, want 1", len(resp.Rules))
	}
}

// ---- activate-refuses-scope-param scenario ----------------

func TestPolicyWriter_Activate_RejectsScopeField(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	body, _ := json.Marshal(map[string]any{
		"policy_version_id": uuid.Must(uuid.NewV4()).String(),
		"activated_by":      "alice",
		// Architecture Sec 5.3.4 + brief: v1 activation
		// is global per deployment; `scope` is NOT a
		// supported field. The handler MUST reject it.
		"scope": "tenant-abc",
	})
	req := httptest.NewRequest(http.MethodPost, VerbActivatePath, bytes.NewReader(body))
	rr := httptest.NewRecorder()
	pw.Activate(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (scope field must be rejected); body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "scope") {
		t.Errorf("400 body=%q does not mention the rejected field; clients can't self-correct", rr.Body.String())
	}
}

func TestPolicyWriter_Activate_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	body, _ := json.Marshal(map[string]any{
		"policy_version_id": uuid.Must(uuid.NewV4()).String(),
		"activated_by":      "alice",
		"typo_field":        "oops",
	})
	req := httptest.NewRequest(http.MethodPost, VerbActivatePath, bytes.NewReader(body))
	rr := httptest.NewRecorder()
	pw.Activate(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400 (unknown fields must be rejected)", rr.Code)
	}
}

// ---- 405 method-not-allowed -------------------------------

func TestPolicyWriter_RejectsGET(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	for path, handler := range map[string]http.HandlerFunc{
		VerbPublishPath:         pw.Publish,
		VerbActivatePath:        pw.Activate,
		VerbPublishRulepackPath: pw.PublishRulepack,
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		handler(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s GET: status=%d, want 405", path, rr.Code)
		}
		if !strings.Contains(rr.Header().Get("Allow"), "POST") {
			t.Errorf("%s GET: Allow=%q, want POST", path, rr.Header().Get("Allow"))
		}
	}
}

// ---- 503 steward-not-wired --------------------------------

func TestPolicyWriter_503WhenStewardNil(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(nil)
	cases := map[string]struct {
		path    string
		body    []byte
		handler http.HandlerFunc
	}{
		"publish":          {VerbPublishPath, samplePublishBody(t), pw.Publish},
		"activate":         {VerbActivatePath, []byte(`{"policy_version_id":"00000000-0000-0000-0000-000000000001","activated_by":"alice"}`), pw.Activate},
		"publish_rulepack": {VerbPublishRulepackPath, samplePublishRulepackBody(t), pw.PublishRulepack},
	}
	for name, c := range cases {
		c := c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, c.path, bytes.NewReader(c.body))
			rr := httptest.NewRecorder()
			c.handler(rr, req)
			if rr.Code != http.StatusServiceUnavailable {
				t.Errorf("nil-steward %s: status=%d, want 503; body=%s", name, rr.Code, rr.Body.String())
			}
		})
	}
}

// ---- 503 no-active-signing-key ----------------------------

func TestPolicyWriter_503WhenNoActiveSigningKey(t *testing.T) {
	t.Parallel()
	res, err := keys.Build(context.Background(), keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: false, // no key minted
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	defer res.Close()
	st, err := steward.New(steward.Config{
		Store:  steward.NewInMemoryStore(),
		Signer: res.Manager,
	})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	pw := NewPolicyWriter(st)

	req := httptest.NewRequest(http.MethodPost, VerbPublishPath, bytes.NewReader(samplePublishBody(t)))
	rr := httptest.NewRecorder()
	pw.Publish(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("no-active-key Publish: status=%d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

// ---- 400 malformed JSON -----------------------------------

func TestPolicyWriter_400OnMalformedJSON(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	req := httptest.NewRequest(http.MethodPost, VerbPublishPath, bytes.NewReader([]byte(`not json`)))
	rr := httptest.NewRecorder()
	pw.Publish(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("malformed JSON: status=%d, want 400", rr.Code)
	}
}

func TestPolicyWriter_400OnTrailingData(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	body := append(samplePublishBody(t), []byte(`{"trailing":"oops"}`)...)
	req := httptest.NewRequest(http.MethodPost, VerbPublishPath, bytes.NewReader(body))
	rr := httptest.NewRecorder()
	pw.Publish(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON: status=%d, want 400", rr.Code)
	}
}

// ---- 400 invalid uuid -------------------------------------

func TestPolicyWriter_Activate_400OnInvalidUUID(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	body := []byte(`{"policy_version_id":"not-a-uuid","activated_by":"alice"}`)
	req := httptest.NewRequest(http.MethodPost, VerbActivatePath, bytes.NewReader(body))
	rr := httptest.NewRecorder()
	pw.Activate(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("invalid uuid: status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
}

// ---- 400 shape validation propagated ----------------------

func TestPolicyWriter_Publish_400OnEmptyRuleRefs(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	body := map[string]any{
		"name":           "default-v1",
		"rule_refs":      []any{},
		"threshold_refs": []any{},
		"refactor_weights": map[string]any{
			"alpha": 0.4, "beta": 0.3, "gamma": 0.2, "delta": 0.1,
			"effort_model_version": "v1.0",
			"window_days":          90,
		},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, VerbPublishPath, bytes.NewReader(raw))
	rr := httptest.NewRecorder()
	pw.Publish(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("empty rule_refs: status=%d, want 400", rr.Code)
	}
}

// ---- 409 duplicate publish_rulepack -----------------------

func TestPolicyWriter_PublishRulepack_409OnDuplicate(t *testing.T) {
	t.Parallel()
	st, _ := buildStewardWithEmptyStore(t)
	pw := NewPolicyWriter(st)
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, VerbPublishRulepackPath, bytes.NewReader(samplePublishRulepackBody(t)))
		rr := httptest.NewRecorder()
		pw.PublishRulepack(rr, req)
		if i == 0 && rr.Code != http.StatusOK {
			t.Fatalf("first publish: status=%d, want 200", rr.Code)
		}
		if i == 1 && rr.Code != http.StatusConflict {
			t.Errorf("second publish (same pack_id+version): status=%d, want 409", rr.Code)
		}
	}
}

// ---- 501 unimplemented verbs ------------------------------

func TestPolicyWriter_UnimplementedVerb501(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"policy.rulepack.add":    VerbRulepackAddPath,
		"policy.rulepack.remove": VerbRulepackRemovePath,
		"policy.override":        VerbOverridePath,
	}
	for verbName, path := range cases {
		verbName, path := verbName, path
		t.Run(verbName, func(t *testing.T) {
			t.Parallel()
			handler := UnimplementedVerb(verbName)
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{}`)))
			rr := httptest.NewRecorder()
			handler(rr, req)
			if rr.Code != http.StatusNotImplemented {
				t.Errorf("%s: status=%d, want 501", verbName, rr.Code)
			}
			var body map[string]string
			if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
				t.Fatalf("%s: response body is not JSON: %v; body=%s", verbName, err, rr.Body.String())
			}
			if body["verb"] != verbName {
				t.Errorf("%s: body.verb=%q, want %q", verbName, body["verb"], verbName)
			}
			if body["error"] != "unimplemented_verb" {
				t.Errorf("%s: body.error=%q, want unimplemented_verb", verbName, body["error"])
			}
		})
	}
}

// ---- 501 reachable via mounted route ----------------------

func TestPolicyWriter_Routes_BannedVerbsMounted(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	mux := pw.Routes()
	for _, path := range []string{VerbRulepackAddPath, VerbRulepackRemovePath, VerbOverridePath} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{}`)))
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotImplemented {
			t.Errorf("%s via Routes(): status=%d, want 501", path, rr.Code)
		}
	}
}

// ---- 200 reachable via mounted route ----------------------

func TestPolicyWriter_Routes_PublishMounted(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	mux := pw.Routes()
	req := httptest.NewRequest(http.MethodPost, VerbPublishPath, bytes.NewReader(samplePublishBody(t)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("publish via Routes(): status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// ---- error-translation table (writeStewardError) ---------

func TestWriteStewardError_Translation(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err  error
		want int
	}{
		"no-signing-key":          {steward.ErrNoActiveSigningKey, http.StatusServiceUnavailable},
		"invalid-request":         {steward.ErrInvalidRequest, http.StatusBadRequest},
		"unknown-policy-version":  {steward.ErrUnknownPolicyVersion, http.StatusBadRequest},
		"unknown-rule-ref":        {steward.ErrUnknownRuleRef, http.StatusBadRequest},
		"unknown-threshold-ref":   {steward.ErrUnknownThresholdRef, http.StatusBadRequest},
		"duplicate-rulepack":      {steward.ErrDuplicateRulePack, http.StatusConflict},
		"duplicate-rule":          {steward.ErrDuplicateRule, http.StatusConflict},
		"generic-error":           {errors.New("boom"), http.StatusInternalServerError},
		"wrapped-invalid-request": {wrapErr(steward.ErrInvalidRequest, "field x is empty"), http.StatusBadRequest},
	}
	for name, c := range cases {
		c := c
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/policy/publish", nil)
			writeStewardError(rr, req, "test", c.err)
			if rr.Code != c.want {
				t.Errorf("%s: status=%d, want %d", name, rr.Code, c.want)
			}
		})
	}
}

func wrapErr(sentinel error, msg string) error {
	return &stewardWrappedErr{sentinel: sentinel, msg: msg}
}

type stewardWrappedErr struct {
	sentinel error
	msg      string
}

func (e *stewardWrappedErr) Error() string { return e.msg + ": " + e.sentinel.Error() }
func (e *stewardWrappedErr) Unwrap() error { return e.sentinel }

// ---- 400 on unknown rule_ref / threshold_ref FK ----------

// TestPolicyWriter_Publish_RejectsUnknownRuleRef -- HTTP-layer
// guard for the rule_refs JSON-FK contract. With an empty
// store (no `policy.publish_rulepack` call), the handler must
// translate `steward.ErrUnknownRuleRef` into a 400 with the
// rule_id/version mentioned in the body.
func TestPolicyWriter_Publish_RejectsUnknownRuleRef(t *testing.T) {
	t.Parallel()
	st, _ := buildStewardWithEmptyStore(t)
	pw := NewPolicyWriter(st)

	req := httptest.NewRequest(http.MethodPost, VerbPublishPath, bytes.NewReader(samplePublishBody(t)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	pw.Publish(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "solid.srp.lcom4_high") {
		t.Errorf("body does not name the offending rule_id; got %q", rr.Body.String())
	}
}

// TestPolicyWriter_Publish_RejectsUnknownThresholdRef -- HTTP-
// layer guard for the threshold_refs FK. Seed the rule (so
// rule_refs validates) and an EMPTY threshold table; include a
// threshold_ref in the body; expect 400.
func TestPolicyWriter_Publish_RejectsUnknownThresholdRef(t *testing.T) {
	t.Parallel()
	st, _ := buildStewardWithEmptyStore(t)
	if _, _, err := st.PublishRulepack(context.Background(), steward.PublishRulepackRequest{
		PackID:        "solid.srp",
		Version:       1,
		DisplayName:   "Single Responsibility",
		DescriptionMD: "",
		Rules: []steward.RuleSpec{
			{RuleID: "solid.srp.lcom4_high", Version: 1,
				PredicateDSL: "lcom4 > 0.7", SeverityDefault: steward.SeverityBlock},
		},
	}); err != nil {
		t.Fatalf("seed rulepack: %v", err)
	}
	pw := NewPolicyWriter(st)

	body := map[string]any{
		"name": "default-v1",
		"rule_refs": []map[string]any{
			{"rule_id": "solid.srp.lcom4_high", "version": 1},
		},
		// Unseeded threshold id -- the handler must reject
		// before signing.
		"threshold_refs": []map[string]any{
			{"threshold_id": uuid.Must(uuid.NewV4()).String()},
		},
		"refactor_weights": map[string]any{
			"alpha": 0.4, "beta": 0.3, "gamma": 0.2, "delta": 0.1,
			"effort_model_version": "v1.0",
			"window_days":          90,
		},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, VerbPublishPath, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	pw.Publish(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "threshold") {
		t.Errorf("body does not mention threshold; got %q", rr.Body.String())
	}
}
