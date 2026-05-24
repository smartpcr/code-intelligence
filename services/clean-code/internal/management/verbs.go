package management

import (
	"encoding/json"
	"errors"
	"fmt"
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
type Handler struct {
	reader *Reader
}

// NewHandler wires h.reader. The reader MAY be nil for
// scaffold-mode bring-ups; the handler then returns 503 on
// every verb call (because the underlying reader returns
// [ErrManagerUnavailable]).
func NewHandler(reader *Reader) *Handler {
	return &Handler{reader: reader}
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
//   - 500: other reader errors.
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
			http.Error(w, fmt.Sprintf("management: list_active: %v", err), http.StatusInternalServerError)
		}
		return
	}
	body := make(listActiveResponse, 0, len(views))
	for _, v := range views {
		body = append(body, wireItem(v))
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(body)
}

// Routes returns an `http.ServeMux` ready to mount onto the
// service's HTTP listener. The composition root can pass this
// directly into `http.Handle` or compose it under a parent
// mux. Stage 5.1 ships one route; later stages add more by
// editing this method.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(VerbListActivePath, h.ListActiveSigningKeys)
	return mux
}
