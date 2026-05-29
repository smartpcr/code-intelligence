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
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/policy/steward"
)

// sampleOverrideBody mints a JSON body the Override handler
// accepts as a happy-path mute payload. The body intentionally
// omits `actor_id` (sourced from the X-OIDC-Subject header)
// and `expires_at` (not part of the v1 schema).
func sampleOverrideBody(t *testing.T) []byte {
	t.Helper()
	body := map[string]any{
		"rule_id": "solid.srp.lcom4_high",
		"scope_filter": map[string]any{
			"repo_id":              "repo-a",
			"scope_kind":           "class",
			"scope_signature_glob": "com.example.legacy.*",
		},
		"mute":   true,
		"reason": "legacy code; planned refactor in Q3",
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal sample override body: %v", err)
	}
	return out
}

// newOverrideRequest builds a POST request with the sample body
// and a valid X-OIDC-Subject header. Tests that want to
// exercise the missing-header path use a hand-built request.
func newOverrideRequest(t *testing.T) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(sampleOverrideBody(t)))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	return r
}

// ---- happy paths ----------------------------------------------------

func TestPolicyWriter_Override_HappyPath_Mute(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	w := httptest.NewRecorder()
	pw.Override(w, newOverrideRequest(t))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	var resp overrideResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, w.Body.String())
	}
	if _, err := uuid.FromString(resp.OverrideID); err != nil {
		t.Errorf("OverrideID=%q is not a uuid: %v", resp.OverrideID, err)
	}
	// Architecture pin: the response carries ONLY override_id.
	// Asserts the wire shape stays minimal -- a future change
	// that adds e.g. "created_at" to the body trips this.
	var bag map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &bag); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if len(bag) != 1 {
		t.Errorf("response body has %d top-level fields, want exactly 1 (override_id); body=%s",
			len(bag), w.Body.String())
	}
}

func TestPolicyWriter_Override_HappyPath_Unmute(t *testing.T) {
	t.Parallel()
	st := buildStewardWithMintedKey(t)
	pw := NewPolicyWriter(st)

	// First POST: mute=true.
	{
		w := httptest.NewRecorder()
		pw.Override(w, newOverrideRequest(t))
		if w.Code != http.StatusOK {
			t.Fatalf("mute status=%d, want 200; body=%s", w.Code, w.Body.String())
		}
	}
	// Sleep > Windows-clock-tick (often 15.6ms) so the
	// second row's CreatedAt is strictly greater. Without
	// this sleep, the in-memory store falls back on uuid
	// tie-break which is non-deterministic.
	time.Sleep(50 * time.Millisecond)

	// Second POST: mute=false, reason empty.
	body := map[string]any{
		"rule_id": "solid.srp.lcom4_high",
		"scope_filter": map[string]any{
			"repo_id":              "repo-a",
			"scope_kind":           "class",
			"scope_signature_glob": "com.example.legacy.*",
		},
		"mute":   false,
		"reason": "",
	}
	buf, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(buf))
	r.Header.Set(OIDCSubjectHeader, "bob@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("unmute status=%d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Verify latest-row-wins via the steward read helper.
	// The candidate signature `com.example.legacy.Bar` lies
	// inside the registered glob `com.example.legacy.*`
	// (Stage 5.3 read semantic from architecture Sec
	// 5.3.6 line 1171).
	latest, ok, err := st.LatestMatchingOverride(context.Background(), "solid.srp.lcom4_high",
		steward.CandidateScope{
			RepoID:    "repo-a",
			ScopeKind: steward.ScopeKindClass,
			Signature: "com.example.legacy.Bar",
		})
	if err != nil {
		t.Fatalf("LatestMatchingOverride: %v", err)
	}
	if !ok {
		t.Fatal("LatestMatchingOverride: ok=false")
	}
	if latest.Mute {
		t.Error("latest.Mute=true after unmute POST; want false")
	}
	if latest.ActorID != "bob@example.com" {
		t.Errorf("latest.ActorID=%q, want bob@example.com (the unmuter)", latest.ActorID)
	}
}

// ---- expires_at rejection ------------------------------------------

// TestPolicyWriter_Override_RejectsExpiresAt is the load-bearing
// test for the tech-spec Sec 10A "mute lifecycle" pin: v1 has
// NO TTL column, and the verb refuses any caller-supplied
// `expires_at` field. Architecture Sec 5.3.6 reinforces -- the
// migration 0003 override table has no expires_at column.
func TestPolicyWriter_Override_RejectsExpiresAt(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	body := map[string]any{
		"rule_id": "solid.srp.lcom4_high",
		"scope_filter": map[string]any{
			"repo_id":              "repo-a",
			"scope_kind":           "class",
			"scope_signature_glob": "com.example.*",
		},
		"mute":       true,
		"reason":     "noise",
		"expires_at": "2099-12-31T23:59:59Z", // not in v1
	}
	buf, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(buf))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "expires_at") {
		t.Errorf("error body=%q does not name the offending field 'expires_at'", w.Body.String())
	}
}

// TestPolicyWriter_Override_RejectsBodyActorID pins the
// "actor_id from header, not body" trust boundary -- a caller
// who tries to spoof the subject by putting `actor_id` in the
// body is rejected via DisallowUnknownFields.
func TestPolicyWriter_Override_RejectsBodyActorID(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	body := map[string]any{
		"rule_id": "solid.srp.lcom4_high",
		"scope_filter": map[string]any{
			"repo_id":              "repo-a",
			"scope_kind":           "class",
			"scope_signature_glob": "com.example.*",
		},
		"mute":     true,
		"reason":   "noise",
		"actor_id": "mallory@evil.com",
	}
	buf, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(buf))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// ---- OIDC subject enforcement --------------------------------------

func TestPolicyWriter_Override_RejectsMissingOIDCSubject(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(sampleOverrideBody(t)))
	// No X-OIDC-Subject header set.
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), OIDCSubjectHeader) {
		t.Errorf("error body=%q does not reference %s header for operators reading the log",
			w.Body.String(), OIDCSubjectHeader)
	}
}

func TestPolicyWriter_Override_RejectsEmptyOIDCSubject(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	for _, blank := range []string{"", "   ", "\t\n"} {
		r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(sampleOverrideBody(t)))
		r.Header.Set(OIDCSubjectHeader, blank)
		w := httptest.NewRecorder()
		pw.Override(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("X-OIDC-Subject=%q: status=%d, want 401; body=%s", blank, w.Code, w.Body.String())
		}
	}
}

// ---- shape validation ----------------------------------------------

func TestPolicyWriter_Override_RejectsMuteTrueEmptyReason(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	body := map[string]any{
		"rule_id": "solid.srp.lcom4_high",
		"scope_filter": map[string]any{
			"repo_id":              "repo-a",
			"scope_kind":           "class",
			"scope_signature_glob": "com.example.*",
		},
		"mute":   true,
		"reason": "   ",
	}
	buf, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(buf))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reason") {
		t.Errorf("error body=%q does not name the offending field 'reason'", w.Body.String())
	}
}

func TestPolicyWriter_Override_RejectsInvalidScopeKind(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	body := map[string]any{
		"rule_id": "solid.srp.lcom4_high",
		"scope_filter": map[string]any{
			"repo_id":              "repo-a",
			"scope_kind":           "module", // not in the canonical set
			"scope_signature_glob": "com.example.*",
		},
		"mute":   true,
		"reason": "noise",
	}
	buf, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(buf))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func TestPolicyWriter_Override_RejectsUnknownRule(t *testing.T) {
	t.Parallel()
	// Empty store -- no rules seeded.
	st, _ := buildStewardWithEmptyStore(t)
	pw := NewPolicyWriter(st)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(sampleOverrideBody(t)))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rule_id") {
		t.Errorf("error body=%q does not name the offending rule_id", w.Body.String())
	}
}

// ---- method + wiring -----------------------------------------------

func TestPolicyWriter_Override_RejectsGET(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	r := httptest.NewRequest(http.MethodGet, VerbMgmtOverridePath, nil)
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d, want 405; body=%s", w.Code, w.Body.String())
	}
}

func TestPolicyWriter_Override_503WhenStewardNil(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(nil)
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(sampleOverrideBody(t)))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestPolicyWriter_Override_AcceptsWithoutSigningKey pins the
// kill-switch contract at the HTTP boundary: the handler does
// not call checkSigningKey, so a steward with no active key
// still serves the verb. The steward-layer unit test
// `TestSteward_Override_NoSigningKeyAccepted` covers the same
// property at the verb layer; this test guards the HTTP layer
// from ever GROWING a precondition that re-couples them.
func TestPolicyWriter_Override_AcceptsWithoutSigningKey(t *testing.T) {
	t.Parallel()
	// Use a stub stewardWriter that ignores keys entirely.
	pw := newPolicyWriterFromInterface(stubStewardOverrideOK{})
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(sampleOverrideBody(t)))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (override is the kill switch and must work without an active signing key); body=%s",
			w.Code, w.Body.String())
	}
}

// stubStewardOverrideOK returns a fixed Override row on every
// call; used for HTTP-layer-only tests that don't need a real
// store + signing-key tree.
type stubStewardOverrideOK struct{}

func (stubStewardOverrideOK) Override(_ context.Context, req steward.OverrideRequest) (steward.Override, error) {
	return steward.Override{
		OverrideID:  uuid.Must(uuid.NewV4()),
		RuleID:      req.RuleID,
		ScopeFilter: req.ScopeFilter,
		Mute:        req.Mute,
		Reason:      req.Reason,
		ActorID:     req.ActorID,
	}, nil
}
func (stubStewardOverrideOK) Publish(_ context.Context, _ steward.PublishRequest) (steward.PolicyVersion, error) {
	panic("stub: Publish must not be reached from the Override handler")
}
func (stubStewardOverrideOK) Activate(_ context.Context, _ steward.ActivateRequest) (steward.PolicyActivation, error) {
	panic("stub: Activate must not be reached from the Override handler")
}
func (stubStewardOverrideOK) PublishRulepack(_ context.Context, _ steward.PublishRulepackRequest) (steward.RulePack, []steward.Rule, error) {
	panic("stub: PublishRulepack must not be reached from the Override handler")
}

func TestPolicyWriter_Override_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, strings.NewReader("{not-json"))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	w := httptest.NewRecorder()
	pw.Override(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestPolicyWriter_Routes_OverrideMounted pins the route table.
// A regression that removes the canonical
// `VerbMgmtOverridePath` mount would silently leave the verb
// unreachable; this test catches it at unit time.
func TestPolicyWriter_Routes_OverrideMounted(t *testing.T) {
	t.Parallel()
	pw := NewPolicyWriter(buildStewardWithMintedKey(t))
	mux := pw.Routes()

	// Mgmt override is mounted and reachable -- a real POST
	// with valid body+header MUST land on the handler (200).
	r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(sampleOverrideBody(t)))
	r.Header.Set(OIDCSubjectHeader, "alice@example.com")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, r)
	if rr.Code != http.StatusOK {
		t.Errorf("mgmt.override via Routes(): status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}

	// The historical 501 stays put -- the rename must
	// continue to surface 501 not 404.
	r2 := httptest.NewRequest(http.MethodPost, VerbOverridePath, bytes.NewReader([]byte(`{}`)))
	rr2 := httptest.NewRecorder()
	mux.ServeHTTP(rr2, r2)
	if rr2.Code != http.StatusNotImplemented {
		t.Errorf("legacy policy.override via Routes(): status=%d, want 501 (rename keeps the 501 alive)", rr2.Code)
	}
}

// TestPolicyWriter_Override_StewardErrTranslation pins the
// error-mapping switch in writeStewardError. A new sentinel
// returned from steward.Override that is NOT mapped would fall
// through to a 500; this test guards the two we explicitly
// mapped at Stage 5.3.
func TestPolicyWriter_Override_StewardErrTranslation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err    error
		status int
	}{
		{steward.ErrInvalidOverride, http.StatusBadRequest},
		{steward.ErrUnknownRule, http.StatusBadRequest},
	}
	for _, tc := range cases {
		pw := newPolicyWriterFromInterface(stubStewardOverrideErr{err: tc.err})
		r := httptest.NewRequest(http.MethodPost, VerbMgmtOverridePath, bytes.NewReader(sampleOverrideBody(t)))
		r.Header.Set(OIDCSubjectHeader, "alice@example.com")
		w := httptest.NewRecorder()
		pw.Override(w, r)
		if w.Code != tc.status {
			t.Errorf("err=%v: status=%d, want %d; body=%s", tc.err, w.Code, tc.status, w.Body.String())
		}
	}
}

// stubStewardOverrideErr is a stewardWriter stub that returns a
// caller-chosen error from Override; the other verbs panic if
// touched (they MUST NOT be reached in this test).
type stubStewardOverrideErr struct {
	err error
}

func (s stubStewardOverrideErr) Override(_ context.Context, _ steward.OverrideRequest) (steward.Override, error) {
	return steward.Override{}, s.err
}

func (s stubStewardOverrideErr) Publish(_ context.Context, _ steward.PublishRequest) (steward.PolicyVersion, error) {
	panic("stub: Publish must not be reached from the Override handler")
}

func (s stubStewardOverrideErr) Activate(_ context.Context, _ steward.ActivateRequest) (steward.PolicyActivation, error) {
	panic("stub: Activate must not be reached from the Override handler")
}

func (s stubStewardOverrideErr) PublishRulepack(_ context.Context, _ steward.PublishRulepackRequest) (steward.RulePack, []steward.Rule, error) {
	panic("stub: PublishRulepack must not be reached from the Override handler")
}

// TestPolicyWriter_Override_SentinelDistinct asserts the two
// override sentinels we map are NOT identical to existing
// sentinels (sanity check on the mapping table).
func TestPolicyWriter_Override_SentinelDistinct(t *testing.T) {
	t.Parallel()
	if errors.Is(steward.ErrInvalidOverride, steward.ErrInvalidRequest) {
		t.Error("ErrInvalidOverride aliases ErrInvalidRequest")
	}
	if errors.Is(steward.ErrUnknownRule, steward.ErrUnknownRuleRef) {
		t.Error("ErrUnknownRule aliases ErrUnknownRuleRef")
	}
}
