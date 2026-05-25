package repo_indexer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/gofrs/uuid"
)

// Path is the canonical HTTP path the Repo Indexer webhook
// handler is mounted at. Re-exported so the composition
// root and tests share one literal.
const Path = "/v1/indexer/webhook"

// MaxBodyBytes caps the size of an incoming webhook body.
// A push-event payload is tiny (a few hundred bytes) so a
// 1 MiB ceiling gives generous headroom while preventing a
// degenerate publisher from blocking the writer.
const MaxBodyBytes = 1 << 20 // 1 MiB

// WebhookPayload is the canonical in-process form of a
// Git push-event webhook the Repo Indexer accepts. The
// wire-format is JSON; field tags match the runbook-side
// publisher contract.
//
// # Why a service-specific shape rather than the raw
// # GitHub / GitLab / Bitbucket event
//
// Stage 3.1 is intentionally git-host-agnostic. The
// composition root (or a thin per-host adapter at the
// edge) is the right place to translate a vendor-specific
// push event into this neutral shape; doing the
// translation inside the Indexer would couple it to a
// single git host and fight the architecture Sec 3.3
// characterisation ("a few lines of glue between a git
// event source and the Catalog table").
type WebhookPayload struct {
	// RepoID is the `clean_code.repo.repo_id` UUID the
	// commit belongs to. The publisher resolves the git
	// remote URL to this UUID via the
	// `mgmt.register_repo` registration step (architecture
	// Sec 6.3).
	RepoID uuid.UUID `json:"repo_id"`
	// SHA is the 40-char commit SHA the publisher
	// observed. The Indexer's structural validation
	// rejects any other shape.
	SHA string `json:"sha"`
	// ParentSHA is the parent commit's SHA. May be omitted
	// (or sent as an empty string) for the first commit of
	// a repo.
	ParentSHA string `json:"parent_sha,omitempty"`
	// CommittedAt is the commit's author/committer
	// timestamp in UTC.
	CommittedAt time.Time `json:"committed_at"`
	// Ref is the optional Git ref that received the push
	// (e.g. `refs/heads/main`). Reserved for the future
	// `repo.default_branch_head` maintenance stage; the
	// Stage 3.1 Indexer does not consume it.
	Ref string `json:"ref,omitempty"`
}

// Response is the JSON envelope the handler emits on a
// successful (200) Indexer run. Mirrors
// [CommitEnsureResult] so the publisher can correlate
// duplicate-no-op deliveries without parsing logs.
type Response struct {
	RepoID         uuid.UUID `json:"repo_id"`
	SHA            string    `json:"sha"`
	CommitInserted bool      `json:"commit_inserted"`
	EventInserted  bool      `json:"event_inserted"`
}

// ErrorBody is the JSON envelope the handler emits on a
// 4xx / 5xx response. The `code` field is one of the
// documented canonical literals (see [classifyError]).
type ErrorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// WebhookHandler is the HTTP-facing adapter that decodes a
// Git push-event POST body and drives the [Indexer]
// end-to-end. Construct via [NewWebhookHandler] (test-only;
// no HMAC) or [NewWebhookHandlerWithHMAC] (production;
// HMAC-SHA256 request verification per tech-spec Sec 8.5).
//
// The handler is stateless past its dependencies; one
// instance handles every concurrent request.
type WebhookHandler struct {
	indexer    *Indexer
	logger     *slog.Logger
	hmacSecret []byte // nil = HMAC verification disabled (tests only)
}

// NewWebhookHandler returns a [WebhookHandler] bound to
// `indexer` with NO HMAC verification. Intended for unit
// tests that exercise handler logic without crafting
// signatures.
//
// The composition root MUST use [NewWebhookHandlerWithHMAC]
// in production -- mounting an HMAC-less webhook on a
// real listener is the very thing tech-spec Sec 8.5
// forbids. PANICS when `indexer` is nil.
func NewWebhookHandler(indexer *Indexer, logger *slog.Logger) *WebhookHandler {
	if indexer == nil {
		panic("repo_indexer: NewWebhookHandler received nil Indexer")
	}
	return &WebhookHandler{
		indexer: indexer,
		logger:  logger,
	}
}

// NewWebhookHandlerWithHMAC returns a [WebhookHandler] that
// verifies every request body against `hmacSecret` using
// HMAC-SHA256 (algorithm pinned by the
// `internal/ingest/webhook` package's `HMACSignatureHeader`,
// which the Repo Indexer reuses verbatim to keep one
// canonical webhook-auth shape across the service).
//
// PANICS when `indexer` is nil OR when `hmacSecret` is
// empty -- both are wiring bugs that must fail loudly at
// startup rather than silently allowing unauthenticated
// traffic.
func NewWebhookHandlerWithHMAC(indexer *Indexer, hmacSecret []byte, logger *slog.Logger) *WebhookHandler {
	if indexer == nil {
		panic("repo_indexer: NewWebhookHandlerWithHMAC received nil Indexer")
	}
	if len(hmacSecret) == 0 {
		panic("repo_indexer: NewWebhookHandlerWithHMAC received empty HMAC secret (use NewWebhookHandler for HMAC-less test wiring)")
	}
	// Copy the secret so a caller mutating the slice cannot
	// silently disable verification post-construction.
	secretCopy := make([]byte, len(hmacSecret))
	copy(secretCopy, hmacSecret)
	return &WebhookHandler{
		indexer:    indexer,
		logger:     logger,
		hmacSecret: secretCopy,
	}
}

// ServeHTTP is the http.Handler entrypoint. Mount via
// `mux.Handle(repo_indexer.Path, handler)` OR via the
// convenience method-form `mux.HandleFunc(Path,
// handler.Webhook)`.
//
// # Request validation order (security-critical)
//
// The order is INTENTIONAL: authentication runs BEFORE any
// payload-shape inspection so an unauthenticated caller
// cannot probe the request contract through 400-vs-401
// differential responses.
//
//  1. Method check (POST only -- cheapest filter; pre-auth
//     because the method name is not sensitive).
//  2. Body size-limited read (1 MiB cap; payload-too-large
//     returns 413 pre-auth).
//  3. HMAC verification (when configured): the body bytes
//     are verified BEFORE the Content-Type header is
//     inspected. An invalid or missing signature returns
//     401 + a structured code.
//  4. Content-Type check: the header is parsed via
//     [mime.ParseMediaType] and must equal
//     `application/json`. Parameters (e.g.
//     `charset=utf-8`) are accepted.
//  5. JSON decode with `DisallowUnknownFields`.
//  6. Indexer.OnNewSHA dispatch.
func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.Webhook(w, r)
}

// Webhook is the method-form of [WebhookHandler.ServeHTTP].
// Exposed so a caller using `mux.HandleFunc` can register
// the handler without converting to an http.Handler value.
func (h *WebhookHandler) Webhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "Webhook accepts POST only", "METHOD_NOT_ALLOWED")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			h.writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("Webhook body exceeds %d-byte limit", MaxBodyBytes),
				"PAYLOAD_TOO_LARGE")
			return
		}
		h.writeError(w, http.StatusBadRequest,
			fmt.Sprintf("reading body: %v", err), "BAD_REQUEST")
		return
	}

	// HMAC verification before any payload-shape inspection.
	if h.hmacSecret != nil {
		sig := r.Header.Get(HMACSignatureHeader)
		if vErr := VerifyHMAC(body, sig, h.hmacSecret); vErr != nil {
			code := classifyHMACError(vErr)
			if h.logger != nil {
				h.logger.Warn("repo_indexer webhook: HMAC verification failed",
					"remote_addr", r.RemoteAddr,
					"code", code,
				)
			}
			h.writeError(w, http.StatusUnauthorized,
				fmt.Sprintf("HMAC verification failed: %v", vErr), code)
			return
		}
	}

	// Parse the Content-Type via mime.ParseMediaType so
	// that `application/json; charset=utf-8` is accepted
	// alongside the bare `application/json` form.
	ct := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ct)
	if mediaType != "application/json" {
		h.writeError(w, http.StatusUnsupportedMediaType,
			fmt.Sprintf("Webhook expects Content-Type: application/json (got %q)", ct),
			"UNSUPPORTED_MEDIA_TYPE")
		return
	}

	var payload WebhookPayload
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&payload); err != nil {
		h.writeError(w, http.StatusBadRequest,
			fmt.Sprintf("decoding JSON body: %v", err), "BAD_REQUEST")
		return
	}

	req := CommitEnsureRequest{
		RepoID:      payload.RepoID,
		SHA:         payload.SHA,
		ParentSHA:   payload.ParentSHA,
		CommittedAt: payload.CommittedAt,
		Ref:         payload.Ref,
	}

	res, runErr := h.indexer.OnNewSHA(r.Context(), req)
	if runErr != nil {
		status, code := classifyError(runErr)
		if h.logger != nil && status >= 500 {
			h.logger.Warn("repo_indexer webhook: writer-side failure",
				"repo_id", payload.RepoID,
				"sha", payload.SHA,
				"err", runErr.Error(),
				"code", code,
			)
		}
		h.writeError(w, status, runErr.Error(), code)
		return
	}

	if h.logger != nil {
		h.logger.Info("repo_indexer webhook: success",
			"repo_id", payload.RepoID,
			"sha", payload.SHA,
			"commit_inserted", res.CommitInserted,
			"event_inserted", res.EventInserted,
		)
	}
	h.writeJSON(w, http.StatusOK, Response{
		RepoID:         payload.RepoID,
		SHA:            payload.SHA,
		CommitInserted: res.CommitInserted,
		EventInserted:  res.EventInserted,
	})
}

// classifyError maps an error returned by
// [Indexer.OnNewSHA] to the HTTP status code and
// operator-facing error code the response should carry.
//
// The closed set of mappings:
//
//   - [ErrZeroRepoID]            -> 400 / EMPTY_REPO_ID
//   - [ErrEmptySHA]              -> 400 / EMPTY_SHA
//   - [ErrInvalidSHA]            -> 400 / INVALID_SHA
//   - [ErrInvalidParentSHA]      -> 400 / INVALID_PARENT_SHA
//   - [ErrZeroCommittedAt]       -> 400 / ZERO_COMMITTED_AT
//   - [ErrCatalogWriterFailure]  -> 500 / WRITER_FAILURE
//   - any other error            -> 500 / INTERNAL_ERROR
func classifyError(err error) (status int, code string) {
	switch {
	case errors.Is(err, ErrZeroRepoID):
		return http.StatusBadRequest, "EMPTY_REPO_ID"
	case errors.Is(err, ErrEmptySHA):
		return http.StatusBadRequest, "EMPTY_SHA"
	case errors.Is(err, ErrInvalidSHA):
		return http.StatusBadRequest, "INVALID_SHA"
	case errors.Is(err, ErrInvalidParentSHA):
		return http.StatusBadRequest, "INVALID_PARENT_SHA"
	case errors.Is(err, ErrZeroCommittedAt):
		return http.StatusBadRequest, "ZERO_COMMITTED_AT"
	case errors.Is(err, ErrCatalogWriterFailure):
		return http.StatusInternalServerError, "WRITER_FAILURE"
	default:
		return http.StatusInternalServerError, "INTERNAL_ERROR"
	}
}

// classifyHMACError maps an HMAC-verifier sentinel to the
// canonical error-code string the 401 response carries.
// Mirrors the closed set used by `internal/ingest/webhook`
// so operators see one canonical taxonomy across every
// webhook the service hosts.
func classifyHMACError(err error) string {
	switch {
	case errors.Is(err, ErrHMACMissingHeader):
		return "HMAC_MISSING_SIGNATURE"
	case errors.Is(err, ErrHMACMalformedHeader):
		return "HMAC_MALFORMED_SIGNATURE"
	case errors.Is(err, ErrHMACSignatureMismatch):
		return "HMAC_SIGNATURE_MISMATCH"
	case errors.Is(err, ErrHMACEmptySecret):
		return "HMAC_EMPTY_SECRET"
	default:
		return "HMAC_INVALID"
	}
}

// writeJSON serialises `body` and writes it with `status`.
func (h *WebhookHandler) writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil && h.logger != nil {
		h.logger.Warn("repo_indexer webhook: encode failed", "err", err.Error())
	}
}

// writeError emits a structured [ErrorBody] under `status`.
func (h *WebhookHandler) writeError(w http.ResponseWriter, status int, msg, code string) {
	h.writeJSON(w, status, ErrorBody{Error: msg, Code: code})
}
