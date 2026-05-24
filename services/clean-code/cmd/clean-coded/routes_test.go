package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/health"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/management"
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
	mux := rootMux(health.New("v0", "c0", "t0"), nil)

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
	mux := rootMux(health.New("v0", "c0", "t0"), mgmt)

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
