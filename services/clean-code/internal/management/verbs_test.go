package management

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
)

// buildManagerWithMintedKey returns a Manager wired to the
// in-memory KMS+Store and a single minted key. Mirrors the
// scaffold-mode startup the composition root performs.
func buildManagerWithMintedKey(t *testing.T) *keys.Manager {
	t.Helper()
	res, err := keys.Build(context.Background(), keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	t.Cleanup(res.Close)
	return res.Manager
}

func TestReader_NilManagerSignalsUnavailable(t *testing.T) {
	t.Parallel()
	r := NewReader(nil)
	_, err := r.ListActiveSigningKeys(context.Background())
	if !errors.Is(err, ErrManagerUnavailable) {
		t.Fatalf("ListActiveSigningKeys: err=%v; want ErrManagerUnavailable", err)
	}
}

func TestReader_ListActiveReturnsViewsFromManager(t *testing.T) {
	t.Parallel()
	m := buildManagerWithMintedKey(t)
	r := NewReader(m)
	views, err := r.ListActiveSigningKeys(context.Background())
	if err != nil {
		t.Fatalf("ListActiveSigningKeys: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("len(views)=%d, want 1", len(views))
	}
	if views[0].Fingerprint == "" {
		t.Error("Fingerprint is empty")
	}
	if views[0].KeyID.IsNil() {
		t.Error("KeyID is nil-uuid")
	}
}

func TestHandler_ListActiveBareJSONArray(t *testing.T) {
	t.Parallel()
	m := buildManagerWithMintedKey(t)
	h := NewHandler(NewReader(m))

	req := httptest.NewRequest(http.MethodGet, VerbListActivePath, nil)
	rr := httptest.NewRecorder()
	h.ListActiveSigningKeys(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	// Body MUST be a bare JSON array `[{...}]` per the Stage
	// 5.1 brief verbatim -- NOT `{"keys": [...]}`. Pin this
	// by decoding into `[]map[string]any`.
	var arr []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &arr); err != nil {
		t.Fatalf("body is not a bare JSON array: %v; body=%s", err, rr.Body.String())
	}
	if len(arr) != 1 {
		t.Fatalf("len(body)=%d, want 1; body=%s", len(arr), rr.Body.String())
	}
	// Required field set per brief verbatim: `key_id`,
	// `fingerprint`, `valid_from`, `valid_until`.
	for _, f := range []string{"key_id", "fingerprint", "valid_from", "valid_until"} {
		if _, ok := arr[0][f]; !ok {
			t.Errorf("missing required field %q in response item; got %v", f, arr[0])
		}
	}
}

func TestHandler_ListActiveEmptyArrayWhenNoKeys(t *testing.T) {
	t.Parallel()
	// Build with MintFirstKeyIfEmpty=false so the cache stays
	// empty -- the verb must still return 200 + `[]`.
	res, err := keys.Build(context.Background(), keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: false,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	defer res.Close()
	h := NewHandler(NewReader(res.Manager))

	req := httptest.NewRequest(http.MethodGet, VerbListActivePath, nil)
	rr := httptest.NewRecorder()
	h.ListActiveSigningKeys(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	body := strings.TrimSpace(rr.Body.String())
	if body != "[]" {
		t.Errorf("empty-keys body=%q, want %q", body, "[]")
	}
}

func TestHandler_ListActiveRejectsPOST(t *testing.T) {
	t.Parallel()
	m := buildManagerWithMintedKey(t)
	h := NewHandler(NewReader(m))
	req := httptest.NewRequest(http.MethodPost, VerbListActivePath, nil)
	rr := httptest.NewRecorder()
	h.ListActiveSigningKeys(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d, want 405", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Errorf("Allow header=%q, want substring GET", allow)
	}
}

func TestHandler_ListActiveReturns503WhenManagerNotWired(t *testing.T) {
	t.Parallel()
	h := NewHandler(NewReader(nil))
	req := httptest.NewRequest(http.MethodGet, VerbListActivePath, nil)
	rr := httptest.NewRecorder()
	h.ListActiveSigningKeys(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil-reader status=%d, want 503; body=%s", rr.Code, rr.Body.String())
	}
}

func TestHandler_RoutesIncludesListActivePath(t *testing.T) {
	t.Parallel()
	m := buildManagerWithMintedKey(t)
	h := NewHandler(NewReader(m))
	mux := h.Routes()
	req := httptest.NewRequest(http.MethodGet, VerbListActivePath, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("mux dispatch: status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
}

// TestHandler_ListActiveResponseTimestampsAreRFC3339 pins the
// JSON timestamp encoding to RFC3339 (Go's default
// time.Time.MarshalJSON output). Operators / dashboards parse
// these strings with a fixed layout; locking the format here
// catches a drift introduced by a future custom MarshalJSON.
func TestHandler_ListActiveResponseTimestampsAreRFC3339(t *testing.T) {
	t.Parallel()
	m := buildManagerWithMintedKey(t)
	h := NewHandler(NewReader(m))

	req := httptest.NewRequest(http.MethodGet, VerbListActivePath, nil)
	rr := httptest.NewRecorder()
	h.ListActiveSigningKeys(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", rr.Code)
	}
	var arr []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &arr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(arr) == 0 {
		t.Fatal("response array is empty")
	}
	vfStr, ok := arr[0]["valid_from"].(string)
	if !ok {
		t.Fatalf("valid_from is not a string: %T", arr[0]["valid_from"])
	}
	if _, err := time.Parse(time.RFC3339Nano, vfStr); err != nil {
		if _, err2 := time.Parse(time.RFC3339, vfStr); err2 != nil {
			t.Fatalf("valid_from=%q not RFC3339(Nano): %v / %v", vfStr, err, err2)
		}
	}
}
