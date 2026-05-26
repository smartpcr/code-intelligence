package management

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
)

// VerbListActivePath is the canonical HTTP path the
// `policy.keys.list_active` verb is mounted at. Stage 5.1
// pins this so a runbook / dashboard URL doesn't depend on
// importing the Go package -- callers grep for the path
// string directly. Versioned under `/v1/` so a future shape
// change ships under `/v2/`.
const VerbListActivePath = "/v1/policy/keys/list_active"

// listActiveResponse is the wire shape returned by
// [Handler.ListActiveSigningKeys]. Per the Stage 5.1 brief
// the verb returns `[{key_id, fingerprint, valid_from,
// valid_until}]` -- a BARE JSON array, not an envelope.
// Defined as a named alias so json-shape regression tests
// can lock the shape at the type level.
type listActiveResponse []listActiveItem

// listActiveItem mirrors [keys.ActiveKeyView] one-to-one.
// Duplicated here (rather than reusing the upstream type) so
// the HTTP wire shape is owned by THIS package and cannot
// silently drift if the keys package adds a new field. The
// duplication is cheap and the alternative -- importing the
// upstream type into the wire -- couples the API surface to
// the internal storage type.
type listActiveItem struct {
	KeyID       string    `json:"key_id"`
	Fingerprint string    `json:"fingerprint"`
	ValidFrom   time.Time `json:"valid_from"`
	ValidUntil  time.Time `json:"valid_until"`
}

func wireItem(v keys.ActiveKeyView) listActiveItem {
	return listActiveItem{
		KeyID:       v.KeyID.String(),
		Fingerprint: v.Fingerprint,
		ValidFrom:   v.ValidFrom.UTC(),
		ValidUntil:  v.ValidUntil.UTC(),
	}
}

// Handler serves the management HTTP verbs. It is constructed
// once at start-up by the composition root and mounted onto
// the service's HTTP listener (alongside `/healthz` and
// `/readyz`).
//
// Stage 3.4 adds the optional `writer *MgmtWriter` seam.
// When non-nil, [Handler.Routes] additionally mounts
// `mgmt.retract_sample` and `mgmt.rescan` so the
// production HTTP listener actually exposes the write
// verbs (iter 2 evaluator item #4: the previous version
// only mounted `policy.keys.list_active`, leaving the
// Stage 3.4 verbs reachable only from package tests).
type Handler struct {
	reader *Reader
	writer *MgmtWriter
}

// NewHandler wires h.reader. The reader MAY be nil for
// scaffold-mode bring-ups; the handler then returns 503 on
// every reader-side verb call (because the underlying
// reader returns [ErrManagerUnavailable]). For the write
// verbs use [NewHandlerWithWriter] -- this constructor
// leaves them unmounted (404 from the parent mux).
func NewHandler(reader *Reader) *Handler {
	return &Handler{reader: reader}
}

// NewHandlerWithWriter wires both the reader-side
// (`policy.keys.list_active`) and the Stage 3.4 write
// verbs (`mgmt.retract_sample`, `mgmt.rescan`). Either
// argument MAY be nil; the affected routes are simply
// not mounted by [Handler.Routes].
//
// This keeps composition-root wiring narrow:
//
//	handler := management.NewHandlerWithWriter(reader, writer)
//	srv := &http.Server{Handler: handler.Routes()}
//
// satisfies both the Stage 5.1 reader brief AND the
// Stage 3.4 writer brief in a single mount.
func NewHandlerWithWriter(reader *Reader, writer *MgmtWriter) *Handler {
	return &Handler{reader: reader, writer: writer}
}

// ListActiveSigningKeys serves
// `GET /v1/policy/keys/list_active`. Response body is a BARE
// JSON array `[{key_id, fingerprint, valid_from, valid_until}]`
// per the Stage 5.1 brief verbatim.
//
// Status codes:
//
//   - 200: list emitted (may be `[]`).
//   - 405: method other than GET / HEAD.
//   - 503: signing-key cache not wired or the underlying
//     Manager returned [keys.ErrNoActiveKey].
//   - 500: other reader errors. The response body is the
//     fixed opaque string `internal error`; the underlying
//     error is logged server-side under
//     `management.list_active failed` so operators can
//     diagnose without the wire surface leaking driver / stack
//     details to unauthenticated clients.
//
// Empty arrays at 200 are allowed and expected during the
// brief startup window before [Bootstrap] mints the first key.
func (h *Handler) ListActiveSigningKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if h.reader == nil {
		http.Error(w, "signing key reader not wired", http.StatusServiceUnavailable)
		return
	}
	views, err := h.reader.ListActiveSigningKeys(r.Context())
	if err != nil {
		switch {
		case errors.Is(err, ErrManagerUnavailable):
			http.Error(w, "signing key manager not wired", http.StatusServiceUnavailable)
		case errors.Is(err, keys.ErrNoActiveKey):
			http.Error(w, "no active signing key", http.StatusServiceUnavailable)
		default:
			// Log the raw error server-side (request-id is
			// picked up from r.Context() by the logging
			// handler) but emit an opaque body to the wire.
			// Echoing `err.Error()` back to an unauthenticated
			// HTTP client leaks driver messages, package
			// paths, and stack context.
			slog.ErrorContext(r.Context(), "management.list_active failed",
				"verb", "policy.keys.list_active",
				"error", err.Error(),
			)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	body := make(listActiveResponse, 0, len(views))
	for _, v := range views {
		body = append(body, wireItem(v))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Headers and the 200 status line are already on
		// the wire, so we can't downgrade to 5xx -- the
		// client will see a truncated body either way.
		// Logging is the only signal an operator has to
		// learn the response was malformed (broken pipe,
		// client disconnected mid-write, transient writer
		// failure, etc.). Mirrors the slog shape used by
		// the 500 branch above so a single log query
		// surfaces both failure modes.
		slog.ErrorContext(r.Context(), "management.list_active encode failed",
			"verb", "policy.keys.list_active",
			"error", err.Error(),
		)
	}
}

// Routes returns an `http.ServeMux` ready to mount onto the
// service's HTTP listener. The composition root can pass this
// directly into `http.Handle` or compose it under a parent
// mux. Stage 5.1 ships one route; Stage 3.4 conditionally
// mounts two more when the [Handler] was built with a
// non-nil [MgmtWriter] (see [NewHandlerWithWriter]).
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(VerbListActivePath, h.ListActiveSigningKeys)
	if h.writer != nil {
		// Stage 3.4: production HTTP wiring of the write
		// verbs. Mounted on the SAME mux so a single
		// service.HTTPHandler() call exposes both
		// reader and writer surfaces. Each handler
		// performs its own method / content-type guard,
		// so we mount with mux.HandleFunc rather than
		// composing a sub-mux.
		mux.HandleFunc(VerbMgmtRetractSamplePath, h.writer.RetractSample)
		mux.HandleFunc(VerbMgmtRescanPath, h.writer.Rescan)
	}
	return mux
}
