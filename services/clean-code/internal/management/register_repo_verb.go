package management

// Stage 6.2 -- mgmt.register_repo HTTP verb.
//
// Architecture pin: Sec 6.3 + impl-plan line 603. The verb
// onboards a new repository into the Catalog sub-store by
// writing one `clean_code.repo` row AND one
// `clean_code.repo_event(kind='registered')` audit row.
// Both writes ride a single [RepoStore.RegisterRepo] call
// so the catalog mutation and the audit append are atomic
// from concurrent observers' perspective.
//
// # Idempotency contract
//
// Per the impl-plan scenario `register-repo-idempotent`
// (line 615): "Given a repo already registered, When
// `mgmt.register_repo` is called with the same URL, Then
// the existing repo_id is returned and no duplicate `repo`
// row appears." The handler returns 200 with `created:false`
// on the idempotent path; no second `registered` event is
// appended.
//
// # Mode handling -- wire accepts both `mode` and `modes`
//
// The brief's signature is `register_repo(repo_url,
// default_branch, modes)` (plural). The schema (architecture
// Sec 5.1.1, migration `0001_catalog_lifecycle.up.sql:154`)
// has a SINGLE `mode` column, the `mgmt.set_mode` verb is
// SINGULAR, and the canonical RepoEvent.kind is
// `mode_changed` (singular). To honour the brief verbatim
// AND keep the wire ergonomic for callers that use the
// natural singular form, the wire body accepts EITHER
// `mode` OR `modes` (both as JSON strings, both carrying
// one value from {"embedded", "linked"}). Supplying BOTH
// is rejected with 400 (ambiguous). Supplying NEITHER
// defers to the schema default
// ([RepoModeEmbedded], per operator pin `ast-mode-default`
// architecture Sec 1.6).
//
// # display_name handling
//
// The schema requires `clean_code.repo.display_name NOT
// NULL`. The brief's verb signature does not include it,
// so the wire makes it OPTIONAL: when omitted, the store
// derives it from the URL's path tail (e.g.
// `https://github.com/org/repo.git` -> "repo"). An
// operator that wants a friendlier label can supply
// `display_name` explicitly.

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// VerbMgmtRegisterRepoPath mounts `mgmt.register_repo` at the
// canonical `/v1/mgmt/...` namespace. Pinned as an exported
// constant so dashboards / runbooks reference the string
// directly.
const VerbMgmtRegisterRepoPath = "/v1/mgmt/register_repo"

// Sentinel errors emitted by the register_repo handler. The
// HTTP layer maps each to the canonical status code.
var (
	// ErrMgmtRegisterRepoEmptyURL is returned when the
	// wire body's `repo_url` is empty / whitespace-only.
	// Mapped to 400.
	ErrMgmtRegisterRepoEmptyURL = ErrRepoStoreEmptyURL

	// ErrMgmtRegisterRepoEmptyDefaultBranch is returned
	// when `default_branch` is empty. Mapped to 400.
	ErrMgmtRegisterRepoEmptyDefaultBranch = ErrRepoStoreEmptyDefaultBranch

	// ErrMgmtRegisterRepoInvalidMode is returned when
	// `mode` is non-empty but outside [AllowedRepoModes].
	// Mapped to 400.
	ErrMgmtRegisterRepoInvalidMode = ErrRepoStoreInvalidMode

	// ErrMgmtRegisterRepoBothModeAndModes is returned
	// when the wire body supplies BOTH `mode` and `modes`.
	// Either alone is valid (the brief uses `modes`; the
	// rest of the system uses singular `mode`), but
	// supplying both is ambiguous. Mapped to 400.
	ErrMgmtRegisterRepoBothModeAndModes = errors.New("management: register_repo body: supply either `mode` (singular) or `modes` (plural, per brief), not both")
)

// registerRepoWireRequest is the inbound wire shape for
// `mgmt.register_repo`. Mirrors the brief's parameter set
// (`repo_url, default_branch, modes`) verbatim AND also
// accepts the natural singular `mode` form -- callers
// pick whichever matches their style. The decoder runs
// with `DisallowUnknownFields` so any stray `actor` /
// `repo_id` field is rejected with 400. `actor` is sourced
// from the `X-OIDC-Subject` header (NOT the body) so a
// caller cannot spoof attribution.
type registerRepoWireRequest struct {
	RepoURL       string `json:"repo_url"`
	DefaultBranch string `json:"default_branch"`
	// Mode is the singular form (matches the column
	// `clean_code.repo.mode` and the `mgmt.set_mode` verb).
	// OPTIONAL on the wire -- empty defers to the schema
	// DEFAULT (`embedded`, per operator pin
	// `ast-mode-default` architecture Sec 1.6).
	Mode string `json:"mode,omitempty"`
	// Modes is the plural form named in the brief's
	// signature `register_repo(repo_url, default_branch,
	// modes)`. Accepted as a JSON STRING (single value),
	// NOT an array -- the schema column is singular. If
	// a caller wants to express "one of the allowed modes"
	// they supply that single value here.
	//
	// Supplying BOTH `mode` and `modes` is ambiguous and
	// rejected with 400.
	Modes string `json:"modes,omitempty"`
	// DisplayName is OPTIONAL on the wire. Empty makes
	// the store derive it from `repo_url`'s path tail.
	DisplayName string `json:"display_name,omitempty"`
}

// registerRepoWireResponse is the wire shape returned by
// [MgmtWriter.RegisterRepo] on success.
type registerRepoWireResponse struct {
	// RepoID is the catalog primary key, either freshly
	// minted (Created=true) or the existing row's id
	// (Created=false).
	RepoID string `json:"repo_id"`
	// Created is `true` iff a new `repo` row was inserted
	// AND a `repo_event(kind='registered')` was appended.
	Created bool `json:"created"`
	// Mode is the effective mode stored on the row. On
	// the idempotent path this reflects the EXISTING
	// row's mode, which may differ from the request's
	// `Mode` -- the caller MUST use `mgmt.set_mode` to
	// change mode on an already-registered repo.
	Mode string `json:"mode"`
}

// RegisterRepo serves `POST /v1/mgmt/register_repo`.
//
// The handler's contract verbatim mirrors the workstream
// brief:
//
//  1. Validate the wire body and the OIDC subject header.
//  2. Delegate to [RepoStore.RegisterRepo] which atomically
//     inserts the `clean_code.repo` row (or returns the
//     existing repo_id on the URL-idempotent path) AND
//     appends the matching `repo_event(kind='registered',
//     payload={repo_url, default_branch, mode, actor})`.
//  3. Return `{repo_id, created, mode}`.
//
// Status codes:
//
//   - 200: row registered (Created=true) or idempotent
//     re-register matched (Created=false); body is
//     [registerRepoWireResponse].
//   - 400: malformed JSON, unknown body fields, or shape
//     validation failure (empty URL / default_branch,
//     invalid mode).
//   - 401: missing or empty `X-OIDC-Subject` header.
//   - 405: method other than POST.
//   - 503: [RepoStore] not wired (composition-root
//     scaffold-mode bring-up did not supply
//     [WithMgmtWriterRepoStore]).
//   - 500: any other internal error; opaque body.
func (w *MgmtWriter) RegisterRepo(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	if w.repoStore == nil {
		http.Error(rw, "register_repo surface not wired", http.StatusServiceUnavailable)
		return
	}
	actor := strings.TrimSpace(r.Header.Get(OIDCSubjectHeader))
	if actor == "" {
		http.Error(rw, fmt.Sprintf("missing or empty %s header (the OIDC subject is required)", OIDCSubjectHeader), http.StatusUnauthorized)
		return
	}
	var wire registerRepoWireRequest
	if !decodeStrict(rw, r, &wire) {
		return
	}

	// Front-load the wire validation so we can return a
	// clear 400 with the offending field name BEFORE
	// touching the store. Each error mirrors the store's
	// sentinel so a caller doing `errors.Is` against the
	// HTTP error body matches the same chain.
	repoURL := strings.TrimSpace(wire.RepoURL)
	if repoURL == "" {
		http.Error(rw, ErrMgmtRegisterRepoEmptyURL.Error(), http.StatusBadRequest)
		return
	}
	defaultBranch := strings.TrimSpace(wire.DefaultBranch)
	if defaultBranch == "" {
		http.Error(rw, ErrMgmtRegisterRepoEmptyDefaultBranch.Error(), http.StatusBadRequest)
		return
	}
	// Resolve the mode from EITHER `mode` (singular) OR
	// `modes` (plural, per brief signature). Supplying both
	// is rejected as ambiguous.
	singular := strings.TrimSpace(wire.Mode)
	plural := strings.TrimSpace(wire.Modes)
	var mode string
	switch {
	case singular != "" && plural != "":
		http.Error(rw, ErrMgmtRegisterRepoBothModeAndModes.Error(), http.StatusBadRequest)
		return
	case singular != "":
		mode = singular
	case plural != "":
		mode = plural
	}
	if mode != "" && !IsAllowedRepoMode(mode) {
		http.Error(rw, fmt.Sprintf("%s: got %q (allowed: %s, %s)", ErrMgmtRegisterRepoInvalidMode.Error(), mode, RepoModeEmbedded, RepoModeLinked), http.StatusBadRequest)
		return
	}

	res, err := w.repoStore.RegisterRepo(r.Context(), RegisterRepoRowRequest{
		RepoURL:       repoURL,
		DefaultBranch: defaultBranch,
		Mode:          mode,
		DisplayName:   strings.TrimSpace(wire.DisplayName),
		Actor:         actor,
	})
	if err != nil {
		writeRepoStoreError(rw, r, "mgmt.register_repo", err, w.logger)
		return
	}

	if w.logger != nil {
		w.logger.InfoContext(r.Context(), "mgmt.register_repo succeeded",
			"verb", "mgmt.register_repo",
			"repo_id", res.RepoID.String(),
			"repo_url", repoURL,
			"default_branch", defaultBranch,
			"mode", res.Mode,
			"created", res.Created,
			"actor", actor,
		)
	}

	writeJSON(rw, r, "mgmt.register_repo", http.StatusOK, registerRepoWireResponse{
		RepoID:  res.RepoID.String(),
		Created: res.Created,
		Mode:    res.Mode,
	})
}