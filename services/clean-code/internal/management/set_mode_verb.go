package management

// Stage 6.2 -- mgmt.set_mode HTTP verb.
//
// Architecture pin: Sec 6.3 + impl-plan line 604. The verb
// flips a repo's AST adapter mode (`embedded` <-> `linked`)
// by atomically UPDATEing `clean_code.repo.mode` AND
// appending one
// `clean_code.repo_event(kind='mode_changed',
// payload={mode, previous_mode, actor})` audit row.
//
// # Idempotency contract
//
// Per the impl-plan scenario `set-mode-emits-event` (line
// 616): "Given a repo at mode `embedded`, When
// `mgmt.set_mode(repo_id, 'linked')` runs, Then a
// `repo_event(kind='mode_changed')` is appended ... and
// subsequent `mgmt.read.repo` returns mode=`linked`."
//
// A call that re-asserts the existing mode is a canonical
// no-op: 200 + `changed:false`, no UPDATE issued, no
// event appended. This matches the architecture's
// "mode_changed records a TRANSITION" reading of Sec
// 5.1.4 line 910 -- a `mode_changed` row that records
// `embedded -> embedded` would be audit noise and would
// distort any "how many real mode flips has this repo
// seen?" query.

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/telemetry"
)

// VerbMgmtSetModePath mounts `mgmt.set_mode` at the canonical
// `/v1/mgmt/...` namespace. Pinned as an exported constant
// so dashboards / runbooks reference the string directly.
const VerbMgmtSetModePath = "/v1/mgmt/set_mode"

// Sentinel errors emitted by the set_mode handler. The HTTP
// layer maps each to the canonical status code.
var (
	// ErrMgmtSetModeZeroRepoID is returned when the wire
	// body's `repo_id` is the zero UUID. Mapped to 400.
	ErrMgmtSetModeZeroRepoID = errors.New("management: set_mode.repo_id is the zero UUID")
	// ErrMgmtSetModeUnknownRepo is returned when the
	// supplied `repo_id` is not present in the catalog.
	// Mapped to 404.
	ErrMgmtSetModeUnknownRepo = errors.New("management: set_mode.repo_id not found")
)

// setModeWireRequest is the inbound wire shape for
// `mgmt.set_mode`. Mirrors the brief verbatim:
// `(repo_id, mode)`. `actor` is sourced from the
// `X-OIDC-Subject` header.
//
// The decoder runs with `DisallowUnknownFields` so any
// stray `payload` / `actor` / `previous_mode` field is
// rejected with 400.
type setModeWireRequest struct {
	RepoID string `json:"repo_id"`
	Mode   string `json:"mode"`
}

// setModeWireResponse is the wire shape returned by
// [MgmtWriter.SetMode] on success.
type setModeWireResponse struct {
	// RepoID echoes the request input.
	RepoID string `json:"repo_id"`
	// Mode is the effective mode AFTER the call.
	Mode string `json:"mode"`
	// PreviousMode is the mode the row carried BEFORE
	// the call. Equal to Mode on the no-op path.
	PreviousMode string `json:"previous_mode"`
	// Changed is `true` iff a transition occurred (UPDATE
	// issued AND a `mode_changed` event appended).
	// `false` iff the row was already at the requested
	// mode (canonical no-op).
	Changed bool `json:"changed"`
}

// SetMode serves `POST /v1/mgmt/set_mode`.
//
// The handler's contract verbatim mirrors the workstream
// brief:
//
//  1. Validate the wire body and the OIDC subject header.
//  2. Delegate to [RepoStore.SetRepoMode] which atomically
//     UPDATEs `clean_code.repo.mode` AND appends the
//     matching `repo_event(kind='mode_changed',
//     payload={mode, previous_mode, actor})`.
//  3. Return `{repo_id, mode, previous_mode, changed}`.
//
// Status codes:
//
//   - 200: mode transitioned (Changed=true) OR was already
//     at the target value (Changed=false, canonical no-op);
//     body is [setModeWireResponse].
//   - 400: malformed JSON, unknown body fields, invalid
//     `repo_id` (not a UUID, or the zero UUID), or mode
//     outside [AllowedRepoModes].
//   - 401: missing or empty `X-OIDC-Subject` header.
//   - 404: `repo_id` not found in `clean_code.repo`.
//   - 405: method other than POST.
//   - 503: [RepoStore] not wired.
//   - 500: any other internal error; opaque body.
func (w *MgmtWriter) SetMode(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	if w.repoStore == nil {
		http.Error(rw, "set_mode surface not wired", http.StatusServiceUnavailable)
		return
	}
	actor := strings.TrimSpace(r.Header.Get(OIDCSubjectHeader))
	if actor == "" {
		http.Error(rw, fmt.Sprintf("missing or empty %s header (the OIDC subject is required)", OIDCSubjectHeader), http.StatusUnauthorized)
		return
	}
	var wire setModeWireRequest
	if !decodeStrict(rw, r, &wire) {
		return
	}

	repoID, err := uuid.FromString(wire.RepoID)
	if err != nil || repoID == uuid.Nil {
		// Distinguish "you sent garbage" from "you sent
		// the zero UUID" so the operator sees the right
		// sentinel echoed back.
		if err != nil {
			http.Error(rw, fmt.Sprintf("invalid repo_id: %s", err.Error()), http.StatusBadRequest)
			return
		}
		http.Error(rw, ErrMgmtSetModeZeroRepoID.Error(), http.StatusBadRequest)
		return
	}
	// Stage 9.4 iter-4: overwrite the verb-span's
	// `repo_id=""` open-time placeholder now that the wire
	// request has parsed and the UUID is non-zero. Safe
	// no-op when no OTel span is bound to the request
	// context.
	telemetry.AnnotateVerbSpanRepoID(r.Context(), repoID.String())
	mode := strings.TrimSpace(wire.Mode)
	if !IsAllowedRepoMode(mode) {
		http.Error(rw, fmt.Sprintf("%s: got %q (allowed: %s, %s)", ErrRepoStoreInvalidMode.Error(), mode, RepoModeEmbedded, RepoModeLinked), http.StatusBadRequest)
		return
	}

	res, err := w.repoStore.SetRepoMode(r.Context(), SetRepoModeRequest{
		RepoID: repoID,
		Mode:   mode,
		Actor:  actor,
	})
	if err != nil {
		writeRepoStoreError(rw, r, "mgmt.set_mode", err, w.logger)
		return
	}

	if w.logger != nil {
		w.logger.InfoContext(r.Context(), "mgmt.set_mode succeeded",
			"verb", "mgmt.set_mode",
			"repo_id", res.RepoID.String(),
			"mode", res.Mode,
			"previous_mode", res.PreviousMode,
			"changed", res.Changed,
			"actor", actor,
		)
	}

	writeJSON(rw, r, "mgmt.set_mode", http.StatusOK, setModeWireResponse{
		RepoID:       res.RepoID.String(),
		Mode:         res.Mode,
		PreviousMode: res.PreviousMode,
		Changed:      res.Changed,
	})
}

// writeRepoStoreError maps a [RepoStore] error to the matching
// HTTP status. Centralised here so both `register_repo` and
// `set_mode` use the same mapping table:
//
//   - [ErrRepoStoreUnknownRepo]         -> 404
//   - [ErrRepoStoreEmptyURL]            -> 400
//   - [ErrRepoStoreEmptyDefaultBranch]  -> 400
//   - [ErrRepoStoreInvalidMode]         -> 400
//   - [ErrRepoStoreZeroRepoID]          -> 400
//   - anything else                      -> 500 + opaque body
//     (raw error logged server-side; wire stays opaque).
//
// The matches use [errors.Is] so a future PG implementation
// that wraps the sentinel with `%w` keeps the same wire
// behaviour.
func writeRepoStoreError(rw http.ResponseWriter, r *http.Request, verb string, err error, log *slog.Logger) {
	switch {
	case errors.Is(err, ErrRepoStoreUnknownRepo):
		http.Error(rw, err.Error(), http.StatusNotFound)
	case errors.Is(err, ErrRepoStoreEmptyURL):
		http.Error(rw, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrRepoStoreEmptyDefaultBranch):
		http.Error(rw, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrRepoStoreInvalidMode):
		http.Error(rw, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrRepoStoreZeroRepoID):
		http.Error(rw, err.Error(), http.StatusBadRequest)
	default:
		if log != nil {
			log.ErrorContext(r.Context(), "management repo_store verb failed",
				"verb", verb,
				"err", err.Error(),
			)
		}
		http.Error(rw, "internal error", http.StatusInternalServerError)
	}
}
