package main

import (
	"net/http"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/health"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/management"
)

// rootMux assembles a single `http.ServeMux` that hosts every
// HTTP route the clean-coded process exposes. Centralising the
// route registration here (rather than mounting two child
// muxes underneath a parent) avoids the http.ServeMux
// catch-all-vs-prefix pitfalls: every path is added once,
// against the same mux, and the test harness can dispatch
// against it directly via `mux.ServeHTTP`.
//
// `mgmt` MAY be nil. When the composition root is in scaffold
// mode (no KMS provider configured) the signing-key cache
// isn't wired, but rootMux STILL mounts the management routes
// against a scaffold handler that emits 503 -- the contract
// pinned by `services/clean-code/docs/runbook.md` (Stage 5.1
// runbook) is that an unwired signing-key cache surfaces as
// `503 Service Unavailable` at `/v1/policy/keys/list_active`,
// NOT `404 Not Found`. 503 tells operators "the verb exists
// here, the backing subsystem is down"; 404 would be
// ambiguous ("does this build even ship the verb?").
// `management.NewHandler(nil)` already handles the nil-reader
// case with a 503 + descriptive body inside
// `ListActiveSigningKeys`, so the scaffold path reuses that
// branch instead of duplicating an inline 503.
func rootMux(healthHandler *health.Handler, mgmt *management.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler.Healthz)
	mux.HandleFunc("/readyz", healthHandler.Readyz)
	if mgmt == nil {
		mgmt = management.NewHandler(nil)
	}
	mux.HandleFunc(management.VerbListActivePath, mgmt.ListActiveSigningKeys)
	return mux
}
