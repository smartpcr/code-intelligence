// Package health implements the `/healthz` and `/readyz` HTTP
// endpoints the clean-code service exposes from the very first
// stage.
//
// Stage 1.1 (implementation-plan.md line 53) carves out the
// contract:
//
//   - `/healthz` -- process liveness. Returns HTTP 200 with a JSON
//     body `{"status":"ok","version":...,"commit":...,"build_time":...}`
//     once the process is up. It is intentionally cheap (no
//     downstream IO) so a load balancer's hot-path probe never
//     drives latency.
//
//   - `/readyz` -- composite readiness. Returns HTTP 200 ONLY
//     when EVERY mandatory readiness gate is registered AND
//     reports nil. The Stage 1.1-mandated trio is the
//     PostgreSQL pool (`postgres`), the OTel exporter
//     (`otel_exporter`), and the signing-key cache
//     (`signing_key_cache`); see `DefaultMandatoryChecks`. A
//     pod that has not finished wiring even one of those
//     dependencies MUST be excluded from traffic. The handler
//     enforces a hard wall-clock timeout via the request
//     context so a probe that ignores ctx.Done() cannot wedge
//     `/readyz`.
//
// Both handlers emit JSON with a strict shape so dashboards /
// load-balancer probes can parse them without scraping HTML.
package health

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// DefaultMandatoryChecks lists the readiness gates the
// implementation-plan.md Stage 1.1 contract names as mandatory:
// `PostgreSQL pool, OTel exporter, and signing-key cache`. The
// `/readyz` handler returns HTTP 503 unless EVERY name in this
// slice is registered AND its check returns nil.
//
// The names are canonical -- callers wiring subsystems into the
// Handler MUST use these exact strings when calling
// `AddReadyCheck` for the corresponding subsystem, otherwise
// `/readyz` will keep reporting the mandatory check as missing.
//
// New mandatory gates added in later stages should be appended
// here (or registered via SetMandatoryChecks) rather than
// snuck in via AddReadyCheck.
var DefaultMandatoryChecks = []string{
	"postgres",
	"otel_exporter",
	"signing_key_cache",
}

// Check is a single readiness probe. It returns nil when the
// dependency is warm and an error otherwise. The context carries
// the deadline the Handler enforces on the aggregate probe (so
// no single hung dependency wedges the readiness response). A
// check that ignores ctx will still be abandoned by Readyz when
// the aggregate timeout fires -- the goroutine continues in the
// background but its result is discarded.
type Check func(ctx context.Context) error

// Handler is the HTTP handler for /healthz and /readyz. It is
// safe for concurrent use; checks may be registered or removed
// dynamically as subsystems come online.
type Handler struct {
	version, commit, buildTime string

	mu        sync.RWMutex
	checks    map[string]Check
	mandatory map[string]struct{}

	// timeout is the max wall-clock the /readyz response will
	// wait for. A check that does not return within this window
	// is reported as not-ready with a "timeout" reason and the
	// handler short-circuits -- it does NOT wait for the stuck
	// goroutine to finish. The default of 2s matches an LB
	// probe budget and is enough headroom for a warm PG ping or
	// a static cache lookup.
	timeout time.Duration
}

// New constructs a Handler. The supplied version metadata is
// echoed verbatim into the /healthz JSON body -- the caller is
// expected to pass the values from `internal/version`. The
// mandatory readiness gates default to `DefaultMandatoryChecks`
// (PostgreSQL pool, OTel exporter, signing-key cache).
func New(version, commit, buildTime string) *Handler {
	h := &Handler{
		version:   version,
		commit:    commit,
		buildTime: buildTime,
		checks:    make(map[string]Check),
		mandatory: make(map[string]struct{}, len(DefaultMandatoryChecks)),
		timeout:   2 * time.Second,
	}
	for _, n := range DefaultMandatoryChecks {
		h.mandatory[n] = struct{}{}
	}
	return h
}

// SetTimeout overrides the per-probe timeout. The default of 2s
// matches an LB probe budget and is enough headroom for a warm
// PG ping or a static cache lookup.
func (h *Handler) SetTimeout(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.timeout = d
}

// SetMandatoryChecks replaces the mandatory-check set. Use this
// to extend the contract in later stages or to disable
// mandatory-gating entirely (pass nil / empty) in test helpers.
// When the mandatory set is empty AND no checks are registered,
// /readyz returns 503 -- a service with no probes wired MUST
// NOT advertise readiness.
func (h *Handler) SetMandatoryChecks(names []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mandatory = make(map[string]struct{}, len(names))
	for _, n := range names {
		if n == "" {
			continue
		}
		h.mandatory[n] = struct{}{}
	}
}

// MandatoryChecks returns the canonical list (sorted) of
// mandatory readiness gates.
func (h *Handler) MandatoryChecks() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.mandatory))
	for k := range h.mandatory {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AddReadyCheck registers a named readiness gate. Reusing a name
// silently replaces the prior check so a subsystem that re-
// initialises (e.g. on PG reconnect) can refresh its probe
// without first removing the stale one.
func (h *Handler) AddReadyCheck(name string, check Check) {
	if name == "" || check == nil {
		return
	}
	h.mu.Lock()
	h.checks[name] = check
	h.mu.Unlock()
}

// RemoveReadyCheck unregisters a named check.
func (h *Handler) RemoveReadyCheck(name string) {
	h.mu.Lock()
	delete(h.checks, name)
	h.mu.Unlock()
}

// ChecksRegistered returns the names of every currently registered
// check. Useful for diagnostics / tests.
func (h *Handler) ChecksRegistered() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.checks))
	for k := range h.checks {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Healthz responds with HTTP 200 + a minimal liveness JSON
// document. It MUST stay cheap -- no IO -- so the probe latency
// is bounded by JSON encoding alone.
func (h *Handler) Healthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body := map[string]string{
		"status":     "ok",
		"version":    h.version,
		"commit":     h.commit,
		"build_time": h.buildTime,
	}
	writeJSON(w, http.StatusOK, body)
}

// readyResult is the per-check entry in the /readyz response.
type readyResult struct {
	Status string `json:"status"`           // ok | not_ready
	Reason string `json:"reason,omitempty"` // populated when Status != ok
}

// Readyz runs every registered check in parallel under a shared
// hard timeout enforced via ctx.Done(). Behaviour:
//
//   - Every name in the mandatory-check set MUST be registered.
//     A missing mandatory check is reported as
//     not_ready/"not registered" and the overall response is 503.
//
//   - Every registered check is invoked with a shared ctx that
//     carries the configured timeout. The handler returns as
//     soon as either (a) every check has reported or (b) the
//     timeout fires -- whichever comes first. Outstanding
//     checks at timeout are reported as not_ready/"timeout"
//     and their goroutines are abandoned (they will eventually
//     return into the buffered result channel and be GC'd).
//
//   - If any check (mandatory or otherwise) returns a non-nil
//     error, the overall response is 503 with the individual
//     reason populated.
//
//   - If no checks are registered AND no mandatory set is
//     configured, /readyz returns 503 -- a service with no
//     probes wired MUST NOT advertise readiness.
func (h *Handler) Readyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	h.mu.RLock()
	checks := make(map[string]Check, len(h.checks))
	for k, v := range h.checks {
		checks[k] = v
	}
	mandatory := make([]string, 0, len(h.mandatory))
	for k := range h.mandatory {
		mandatory = append(mandatory, k)
	}
	sort.Strings(mandatory)
	timeout := h.timeout
	h.mu.RUnlock()

	results := make(map[string]readyResult, len(checks)+len(mandatory))
	allOK := true

	// Step 1: mandatory-check coverage. Any mandatory gate that
	// is not registered fails the response synchronously --
	// there is no point starting a goroutine for a check that
	// does not exist.
	for _, name := range mandatory {
		if _, ok := checks[name]; !ok {
			results[name] = readyResult{Status: "not_ready", Reason: "not registered"}
			allOK = false
		}
	}

	// Step 2: run every registered probe in parallel under a
	// shared hard deadline. We use a select on ctx.Done() so a
	// hung probe that ignores ctx cannot wedge the handler.
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	type outcome struct {
		name string
		err  error
	}
	// Buffered so an abandoned goroutine can still complete its
	// send and exit cleanly (no leak), even after Readyz has
	// returned to the caller.
	out := make(chan outcome, len(checks))
	for name, check := range checks {
		go func(name string, check Check) {
			defer func() {
				if rec := recover(); rec != nil {
					out <- outcome{name: name, err: panicErr(rec)}
				}
			}()
			out <- outcome{name: name, err: check(ctx)}
		}(name, check)
	}

	pending := len(checks)
collect:
	for pending > 0 {
		select {
		case o := <-out:
			pending--
			if o.err == nil {
				results[o.name] = readyResult{Status: "ok"}
			} else {
				results[o.name] = readyResult{Status: "not_ready", Reason: o.err.Error()}
				allOK = false
			}
		case <-ctx.Done():
			// Hard timeout: abandon every still-pending check
			// without waiting for its goroutine to return.
			for name := range checks {
				if _, seen := results[name]; !seen {
					results[name] = readyResult{Status: "not_ready", Reason: "timeout: " + ctx.Err().Error()}
					allOK = false
				}
			}
			break collect
		}
	}

	// Step 3: determine final status. The "empty + no
	// mandatory" branch keeps the scaffold-service invariant
	// that an unwired process MUST NOT advertise readiness.
	status := http.StatusOK
	overall := "ok"
	if len(checks) == 0 && len(mandatory) == 0 {
		status = http.StatusServiceUnavailable
		overall = "not_ready"
	} else if !allOK {
		status = http.StatusServiceUnavailable
		overall = "not_ready"
	}
	body := map[string]any{
		"status": overall,
		"checks": results,
	}
	writeJSON(w, status, body)
}

// Routes returns the http.ServeMux routes a caller should mount
// to expose `/healthz` and `/readyz`. Useful when the caller has
// its own ServeMux and wants to register handlers explicitly
// without referring to the Handler's method values.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.Healthz)
	mux.HandleFunc("/readyz", h.Readyz)
	return mux
}

// writeJSON emits a JSON response with the supplied status code.
// Errors during encoding are intentionally swallowed -- the
// response has already begun streaming and there is nowhere
// useful to surface a marshal error.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// panicErr wraps the recovered value as an error so a check
// implementation that panics is reported as a not-ready
// outcome rather than killing the response goroutine.
func panicErr(v any) error {
	return &checkPanicError{value: v}
}

type checkPanicError struct {
	value any
}

func (e *checkPanicError) Error() string {
	return "check panicked"
}
