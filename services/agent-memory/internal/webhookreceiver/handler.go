package webhookreceiver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lib/pq"
)

// pgErrCodeInvalidTextRepresentation is the SQLSTATE PostgreSQL
// returns when a text value can not be cast to the requested
// type (class 22, code P02). For this package the canonical
// trigger is the `$1::uuid` cast in `lookupSecret` against a
// non-UUID URL segment. Pulled out as a constant so the
// classifier and any future test fixture share the same literal
// rather than scattering "22P02" through the code.
const pgErrCodeInvalidTextRepresentation = "22P02"

// DefaultMaxBodyBytes caps the inbound webhook body so a
// pre-auth attacker cannot exhaust memory before signature
// verification runs. 1 MiB is comfortably larger than any real
// push payload (GitHub's is typically < 50 KiB even on a deep
// fork-point delivery) and small enough that an O(1000)
// concurrent request flood costs ~1 GiB of resident memory at
// worst.
const DefaultMaxBodyBytes int64 = 1 << 20

// DefaultSignatureHeader is the GitHub convention -- value is
// `sha256=<lowercase-hex>` of the HMAC-SHA256 over the raw
// body. Bitbucket and most self-hosted Gitea / Forgejo
// deployments accept the same shape. Operators wanting GitLab
// support add a separate adapter; this stage standardises on
// the GitHub format per the iter-1 design pass.
const DefaultSignatureHeader = "X-Hub-Signature-256"

// signaturePrefix is the leading literal that every
// X-Hub-Signature-256 header carries -- GitHub spec. Pulled
// out as a constant so the verifier and the documentation
// can not drift apart.
const signaturePrefix = "sha256="

// RoutePrefix is the URL path prefix the handler binds to.
// `cmd/webhook-receiver/main.go` mounts the handler at this
// prefix; the trailing repo_id segment is parsed out by
// `extractRepoID`. Exposed (not a private constant) so the
// cmd binary and the integration tests reference the same
// symbol.
const RoutePrefix = "/webhook/"

// gitSHA1HexLen is the length of a SHA-1 git object id rendered as
// lowercase hex: 160 bits / 4 bits per hex char = 40 chars. This is
// the historical (and still overwhelmingly common) git object-id
// width.
const gitSHA1HexLen = 40

// gitSHA256HexLen is the length of a SHA-256 git object id rendered
// as lowercase hex: 256 bits / 4 = 64 chars. Repos created with
// `git init --object-format=sha256` (or hosts that have migrated)
// use this width; the wire format from those hosts is still hex.
const gitSHA256HexLen = 64

// Allowed `kind` values on inbound webhook bodies. Mirrors
// the `repo_event_kind` ENUM subset that a git host can
// legitimately fire: `register` / `manual` come from `mgmt.*`
// verbs and MUST NOT be accepted via this surface.
//
// Maintained as a closed set rather than a generic "is this a
// valid ENUM label" predicate so a typo can not silently
// promote a write into the wrong code path.
var allowedKinds = map[string]struct{}{
	"push":  {},
	"merge": {},
}

// Payload is the canonical body shape the handler decodes from
// an authenticated webhook. The JSON wire format is stable
// (lower-snake-case field names) so a future git-host adapter
// can rewrite a vendor payload into this shape without
// touching the verifier.
//
// `repo_id` from the body is intentionally NOT a field on this
// struct -- the URL path is the authoritative repo identity.
// Allowing a body-level `repo_id` would create a confused-deputy
// surface where the URL says one repo but the audit row says
// another.
type Payload struct {
	Kind    string `json:"kind"`
	FromSHA string `json:"from_sha,omitempty"`
	ToSHA   string `json:"to_sha"`
}

// Response is the JSON envelope every 202 Accepted reply
// carries. `event_id` is the freshly-minted `repo_event` PK;
// `job_id` is the `ingest_jobs` PK (which may be a brand new
// row OR the existing row from a deduped retry). Stable shape
// so an operator's `curl | jq '.job_id'` keeps working as the
// service evolves.
type Response struct {
	EventID  string `json:"event_id"`
	JobID    string `json:"job_id"`
	JobState string `json:"job_state"`
}

// Clock abstracts time.Now so tests can fix the timestamp on
// rows the handler writes. Production passes a real clock via
// HandlerOptions.Clock; tests inject a `func() time.Time` that
// returns a frozen value.
type Clock func() time.Time

// Options bundles the optional knobs callers can pass to
// `NewHandler`. Every field has a sensible default if the
// caller leaves it zero-valued -- the goal is that the cmd
// binary can construct a working handler with just a *sql.DB.
type Options struct {
	// Logger receives one structured record per request. Defaults
	// to slog.Default(). The handler NEVER logs the webhook
	// secret, the raw signature header, or the raw request body.
	Logger *slog.Logger
	// SignatureHeader is the HTTP header the handler reads the
	// HMAC from. Defaults to DefaultSignatureHeader. Configurable
	// so a private deployment can fence off the public-facing
	// header name and route via a reverse proxy.
	SignatureHeader string
	// MaxBodyBytes caps the request body the handler will read.
	// Zero means DefaultMaxBodyBytes. Negative disables the cap
	// (NOT recommended in production -- exposes a memory-DoS
	// surface).
	MaxBodyBytes int64
	// Clock is the time source used for the structured-log
	// `received_at`. Database `received_at` columns use
	// PostgreSQL's `now()` so a fixed Go-side clock does NOT
	// fix the DB timestamp; production callers leave this nil
	// (defaults to time.Now) and tests that need a frozen log
	// timestamp inject one.
	Clock Clock
}

// Handler is the HTTP handler that authenticates and enqueues
// webhook events. Construct one per process via NewHandler;
// the same handler instance is safe for concurrent use.
//
// Fields are intentionally unexported -- everything callers
// need (Options to construct, ServeHTTP to invoke) is on the
// public surface above.
type Handler struct {
	db              *sql.DB
	logger          *slog.Logger
	signatureHeader string
	maxBodyBytes    int64
	clock           Clock
}

// NewHandler builds a Handler over `db`. Panics on a nil
// *sql.DB: a webhook receiver without a backing database has
// nothing useful to do (it can not verify the secret OR write
// the audit row), so this is a programmer error worth crashing
// the process for.
func NewHandler(db *sql.DB, opts Options) *Handler {
	if db == nil {
		panic("webhookreceiver: NewHandler: nil *sql.DB")
	}
	h := &Handler{
		db:              db,
		logger:          opts.Logger,
		signatureHeader: opts.SignatureHeader,
		maxBodyBytes:    opts.MaxBodyBytes,
		clock:           opts.Clock,
	}
	if h.logger == nil {
		h.logger = slog.Default()
	}
	if h.signatureHeader == "" {
		h.signatureHeader = DefaultSignatureHeader
	}
	if h.maxBodyBytes == 0 {
		h.maxBodyBytes = DefaultMaxBodyBytes
	}
	if h.clock == nil {
		h.clock = time.Now
	}
	return h
}

// ServeHTTP routes inbound requests under RoutePrefix to the
// authenticated handler. Anything else returns 404 so the
// surface is the minimum a webhook caller needs.
//
// Method gating: only POST is meaningful for a webhook delivery
// (push and merge events are pushed, never pulled). GET / HEAD
// / DELETE / etc return 405 with an empty body -- a healthz
// endpoint, if any, is mounted by the cmd binary on a
// different path.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, RoutePrefix) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	h.handle(w, r)
}

// handle is the unauthenticated request handler. It runs the
// full pipeline -- body read, signature verify, payload parse,
// DB write, response -- and emits one structured log record
// per request. Errors are surfaced to the caller via plain
// `http.Error` for compactness; the structured log carries the
// full diagnostic detail an operator needs.
func (h *Handler) handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	repoID, ok := extractRepoID(r.URL.Path)
	if !ok {
		// Same response shape as a bad-signature request so an
		// unauthenticated probe can not distinguish "unknown
		// repo" from "wrong signature". The 401 is a uniform
		// "unauthorized" surface.
		h.reject(w, "")
		return
	}

	// Cap the body BEFORE reading it. http.MaxBytesReader
	// hooks into the response writer so an over-limit body
	// triggers a 413 automatically; we only see the
	// `*http.MaxBytesError` on io.ReadAll if it overshoots.
	if h.maxBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		// http.MaxBytesReader sets the response status via the
		// `MaxBytesError` sentinel; the caller is responsible
		// for writing the response. We don't have a body or a
		// signature to verify in this case, so we go straight
		// to a 413.
		var mxErr *http.MaxBytesError
		if errors.As(err, &mxErr) {
			h.logger.Warn("webhookreceiver.body_too_large",
				slog.String("repo_id", repoID),
				slog.Int64("limit_bytes", h.maxBodyBytes),
			)
			http.Error(w, "request body too large",
				http.StatusRequestEntityTooLarge)
			return
		}
		h.logger.Warn("webhookreceiver.body_read_failed",
			slog.String("repo_id", repoID),
			slog.String("error", err.Error()),
		)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	sigHeader := r.Header.Get(h.signatureHeader)
	if !h.verifySignature(ctx, repoID, sigHeader, body) {
		// One uniform log record per rejected request -- no
		// secret, no signature header, no body. The
		// structured-log shape mirrors `repoindexer.publisher`
		// so operators see a uniform `{op, repo_id, error}`
		// across both surfaces.
		h.logger.Warn("webhookreceiver.signature_rejected",
			slog.String("op", "webhook"),
			slog.String("repo_id", repoID),
		)
		h.reject(w, repoID)
		return
	}

	var p Payload
	if err := json.Unmarshal(body, &p); err != nil {
		h.logger.Warn("webhookreceiver.payload_invalid",
			slog.String("op", "webhook"),
			slog.String("repo_id", repoID),
			slog.String("error", err.Error()),
		)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := p.validate(); err != nil {
		h.logger.Warn("webhookreceiver.payload_invalid",
			slog.String("op", "webhook"),
			slog.String("repo_id", repoID),
			slog.String("error", err.Error()),
		)
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	resp, err := h.enqueue(ctx, repoID, p)
	if err != nil {
		// We DELIBERATELY do not surface the raw error string
		// to the HTTP caller -- it can include `pq.Error`
		// detail that names columns, constraints, schemas. The
		// structured log gets the full error; the response
		// gets a fixed "internal error" string.
		h.logger.Error("webhookreceiver.enqueue_failed",
			slog.String("op", "webhook"),
			slog.String("repo_id", repoID),
			slog.String("kind", p.Kind),
			slog.String("from_sha", p.FromSHA),
			slog.String("to_sha", p.ToSHA),
			slog.String("error", err.Error()),
		)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	// json.NewEncoder().Encode appends a trailing newline; that
	// matches the http.Error convention and keeps `curl` output
	// readable.
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Body already started; just log -- the caller will
		// see a truncated response which is the only signal we
		// can deliver post-WriteHeader.
		h.logger.Warn("webhookreceiver.response_encode_failed",
			slog.String("repo_id", repoID),
			slog.String("error", err.Error()),
		)
		return
	}
	h.logger.Info("webhookreceiver.enqueued",
		slog.String("op", "webhook"),
		slog.String("repo_id", repoID),
		slog.String("kind", p.Kind),
		slog.String("event_id", resp.EventID),
		slog.String("job_id", resp.JobID),
		slog.String("job_state", resp.JobState),
		slog.Time("received_at", h.clock()),
	)
}

// validate is the payload-level closed-set check. Runs AFTER
// signature verification so a 400 is only observable to a
// caller that already authenticated -- prevents an
// unauthenticated probe from distinguishing 400 from 401.
//
// SHA shape is enforced here: `to_sha` (and `from_sha`, when
// present) MUST be exactly 40 or 64 lowercase hex characters,
// matching the git SHA-1 / SHA-256 object-id widths. Without
// this check, an authenticated caller could inject arbitrary
// strings (shell metacharacters, refspec syntax, junk) into
// `repo_event.to_sha` / `ingest_jobs.to_sha`, which the Stage
// 3.4 delta worker will eventually hand to `git fetch`.
// Enforcing a strict format here is the cheapest place to stop
// confused-deputy / typo-class misuse and limits the blast
// radius of a compromised webhook secret. Lowercase-only is
// intentional: the `ingest_jobs` unique index keys on the raw
// `(from_sha, to_sha)` columns, so accepting mixed case would
// silently bypass the dedupe guard; every git host we target
// (GitHub, GitLab, Bitbucket, Gitea) already emits lowercase
// hex on the wire.
//
// Error strings never echo the attacker-controlled value back
// to the HTTP caller -- they describe the expected shape only.
// The structured log captures the rejected payload separately.
func (p Payload) validate() error {
	if _, ok := allowedKinds[p.Kind]; !ok {
		// Closed set; the DB ENUM would also reject this but
		// we'd rather return 400 than 500. Listing the allowed
		// values in the error string is fine -- it's not a
		// secret.
		return fmt.Errorf("kind must be one of [push merge], got %q", p.Kind)
	}
	if p.ToSHA == "" {
		// `to_sha` is the only required SHA: a delta job's
		// `from_sha` may be empty (an initial push from an
		// orphan branch has no parent), but we always need a
		// destination. The DB has the same `NOT NULL`.
		return errors.New("to_sha is required")
	}
	if !isValidGitSHA(p.ToSHA) {
		return errors.New("to_sha must be a 40 or 64 character lowercase hex git object id")
	}
	if p.FromSHA != "" && !isValidGitSHA(p.FromSHA) {
		return errors.New("from_sha must be empty or a 40 or 64 character lowercase hex git object id")
	}
	return nil
}

// isValidGitSHA reports whether s is exactly 40 or 64 lowercase
// hex characters -- the two widths git uses for object ids
// (SHA-1 and SHA-256 respectively).
//
// Lowercase-only is deliberate: the `ingest_jobs` dedupe index
// is over the raw column bytes, so accepting upper-case here
// would let the same commit register as two distinct jobs
// (`ABCD...` vs `abcd...`). Every git host we target emits
// lowercase on the wire, so a strict check costs nothing in
// practice and removes the dedupe-bypass surface.
//
// Empty is NOT valid here; callers that allow an empty field
// (e.g. `from_sha` on an orphan-branch push) must short-circuit
// before delegating to this helper.
func isValidGitSHA(s string) bool {
	if len(s) != gitSHA1HexLen && len(s) != gitSHA256HexLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// extractRepoID pulls the trailing segment out of
// `RoutePrefix + <uuid>`. Trailing slashes are stripped so
// `/webhook/<id>/` and `/webhook/<id>` are equivalent; deeper
// nested paths (e.g. `/webhook/<id>/foo`) are rejected
// because they're not part of the published contract.
//
// The handler does NOT validate that the extracted value is a
// well-formed UUID -- that's left to the per-request DB lookup
// which returns "no rows" for any non-UUID input anyway. We
// avoid the extra parse so a future migration that switches
// to a different ID shape (e.g. string slugs) does not need
// to touch this helper.
func extractRepoID(path string) (string, bool) {
	tail := strings.TrimPrefix(path, RoutePrefix)
	tail = strings.TrimSuffix(tail, "/")
	if tail == "" || strings.Contains(tail, "/") {
		return "", false
	}
	return tail, true
}

// verifySignature is the HMAC-SHA256 check per tech-spec §9.12.
// Returns true ONLY if all of the following hold:
//
//   - `repoID` resolves to a row in `repo_webhook_secret`.
//   - `header` is a non-empty `sha256=<hex>` value.
//   - The decoded HMAC bytes match HMAC-SHA256(secret, body)
//     under hmac.Equal (constant-time).
//
// Any failure returns false WITHOUT distinguishing between
// "unknown repo", "missing header", "malformed header", and
// "wrong secret" -- callers see a uniform 401.
//
// Database errors propagate AS rejection (return false) and
// are logged at WARN level inside this method, so a DB outage
// surfaces in operator dashboards without exposing detail to
// the caller. This is intentional defence-in-depth: a fail-open
// posture under DB outage would let an attacker exploit a
// momentary `pg_bouncer` blip to land a fake event.
//
// Secret lifetime: `lookupSecret` returns a freshly-allocated
// []byte so we can `clear()` the cleartext bytes as soon as
// the HMAC has been computed and compared. This shortens the
// window in which a memory-read primitive (core dump,
// /proc/pid/mem, future swap-to-disk) could harvest the
// secret on a long-lived process. The hmac.Hash retains only
// derived ipad/opad state, not the raw secret, and goes out
// of scope at function return.
func (h *Handler) verifySignature(ctx context.Context, repoID, header string, body []byte) bool {
	// Strip the `sha256=` prefix; case-insensitive on the
	// prefix per GitHub's published example (real-world
	// senders sometimes upper-case).
	lower := strings.ToLower(header)
	if !strings.HasPrefix(lower, signaturePrefix) {
		return false
	}
	// Use the lower-cased remainder; hex.DecodeString accepts
	// either case but normalising removes one edge case from
	// the verifier.
	provided, err := hex.DecodeString(lower[len(signaturePrefix):])
	if err != nil || len(provided) == 0 {
		return false
	}

	secret, err := h.lookupSecret(ctx, repoID)
	if err != nil {
		h.logger.Warn("webhookreceiver.secret_lookup_failed",
			slog.String("repo_id", repoID),
			slog.String("error", err.Error()),
		)
		return false
	}
	// Zero the cleartext on every return path -- including the
	// "no such repo" branch below. `clear` on a nil/empty slice
	// is a documented no-op (Go 1.21+), so the defer is safe to
	// register before the length check.
	defer clear(secret)
	if len(secret) == 0 {
		// Either the repo is not registered OR mgmt.register
		// has not yet been called for it. Same uniform reject.
		return false
	}

	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body) // hash.Hash.Write never returns an error
	expected := mac.Sum(nil)

	// hmac.Equal is constant-time over EQUAL-LENGTH inputs and
	// returns false if the lengths differ. The earlier
	// hex.DecodeString already pinned `provided` to whatever
	// length the sender supplied; a SHA-256 mismatch (32-byte
	// expected vs N-byte provided where N != 32) thus reduces
	// to a length-aware false.
	return hmac.Equal(expected, provided)
}

// lookupSecret returns the per-repo HMAC secret. Returns a
// nil slice with nil error when the repo is unknown so the
// caller does not have to distinguish sql.ErrNoRows from other
// DB errors -- only true DB outages propagate as a non-nil
// error.
//
// The secret is returned as []byte (not string) so the caller
// can `clear()` the underlying buffer once the HMAC has been
// computed. database/sql guarantees that Scan into *[]byte
// produces a freshly-allocated copy of the column bytes (see
// the database/sql package docs on Rows.Scan), so mutating /
// zeroing the returned slice is safe and does not corrupt any
// driver-internal state. This is defence-in-depth against a
// memory-read primitive harvesting accumulated secrets from
// the GC heap of a long-running process.
func (h *Handler) lookupSecret(ctx context.Context, repoID string) ([]byte, error) {
	const q = `SELECT webhook_secret FROM repo_webhook_secret WHERE repo_id = $1::uuid`
	var secret []byte
	err := h.db.QueryRowContext(ctx, q, repoID).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		// Includes the case where `repoID` is not a valid UUID:
		// PostgreSQL returns SQLSTATE 22P02 (invalid_text_repr)
		// on the `$1::uuid` cast rather than no-rows. We treat
		// that the same as "no such repo" so a malformed URL is
		// still a uniform 401. We still log the error at WARN
		// level inside verify so a flood of malformed requests
		// is observable.
		if isInvalidUUIDError(err) {
			return nil, nil
		}
		return nil, err
	}
	return secret, nil
}

// isInvalidUUIDError reports whether `err` is a PostgreSQL
// invalid-text-representation diagnostic (SQLSTATE 22P02). For
// this package the only practical trigger is the `$1::uuid`
// cast in `lookupSecret` failing on a non-UUID URL segment; we
// classify the cast failure as "no such repo" so a non-UUID URL
// path returns 401 the same way a UUID-shaped but unknown id
// does, instead of leaking a 500.
//
// Classification is done by SQLSTATE, NOT by substring matching
// on the error message. The wire-protocol SQLSTATE is stable
// across PostgreSQL versions and server `lc_messages` locales,
// whereas the rendered message ("invalid input syntax for type
// uuid") is English-only and has drifted historically. Two
// driver shapes are accepted so this function does not need to
// change if the package ever migrates from `lib/pq` to `pgx`:
//
//   - `*pq.Error` exposes the SQLSTATE on its `Code` field.
//     This is the primary path -- it is what `database/sql`
//     surfaces today via the registered `postgres` driver.
//   - Any error satisfying `interface{ SQLState() string }` is
//     also matched. `*pgconn.PgError` (pgx v5) implements this
//     interface, so a future driver swap keeps working without
//     a re-classification edit here.
//
// Returns false for `nil`, for non-Postgres errors, and for
// Postgres errors with a different SQLSTATE -- those propagate
// to the caller as opaque 500s, which is the correct behaviour
// (an unexpected DB error should not be silently downgraded to
// 401 "unauthorized").
func isInvalidUUIDError(err error) bool {
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return string(pqErr.Code) == pgErrCodeInvalidTextRepresentation
	}
	var stateErr interface{ SQLState() string }
	if errors.As(err, &stateErr) {
		return stateErr.SQLState() == pgErrCodeInvalidTextRepresentation
	}
	return false
}

// enqueue writes the `repo_event` audit row and the
// `ingest_jobs` delta row atomically. Returns the freshly-
// minted Response so the HTTP handler can serialise it to JSON.
//
// Both INSERTs share one transaction so a partial failure
// (e.g. the unique-index conflict path raising a transient
// serialisation error) rolls back BOTH rows. We do NOT pre-
// check existence: ON CONFLICT on the ingest_jobs unique
// index is the canonical idempotent-enqueue pattern.
func (h *Handler) enqueue(ctx context.Context, repoID string, p Payload) (Response, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return Response{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // commit path makes rollback a no-op; best-effort cleanup otherwise

	// `from_sha` is `text` NULL in the schema (migration 0006).
	// An empty string from the body means "no parent" -- store
	// it as SQL NULL so the partial-index / dedupe COALESCE
	// matches the existing convention. `pq` maps an untyped
	// `nil` interface to NULL.
	var fromSHA any = p.FromSHA
	if p.FromSHA == "" {
		fromSHA = nil
	}

	var eventID string
	const insertEvent = `
		INSERT INTO repo_event (repo_id, kind, from_sha, to_sha)
		VALUES ($1::uuid, $2::repo_event_kind, $3, $4)
		RETURNING event_id::text
	`
	if err := tx.QueryRowContext(ctx, insertEvent,
		repoID, p.Kind, fromSHA, p.ToSHA,
	).Scan(&eventID); err != nil {
		return Response{}, fmt.Errorf("insert repo_event: %w", err)
	}

	// ON CONFLICT target MUST mirror the expression index
	// shape from migration 0006a:
	//
	//   CREATE UNIQUE INDEX ingest_jobs_dedupe_uidx
	//     ON ingest_jobs (repo_id, mode, COALESCE(from_sha, ''), to_sha);
	//
	// PostgreSQL requires expression entries in the conflict
	// target to be parenthesised separately from the simple
	// column entries -- hence `(COALESCE(from_sha, ''))`.
	//
	// The DO UPDATE clause RESURRECTS a row that previously
	// reached `failed`. Without this, a transient indexer
	// failure followed by a webhook retry would re-deliver the
	// same job_id but leave it parked in `failed`, never to be
	// claimed again. Done / pending / running / claimed rows
	// are left alone (the CASE preserves the existing status)
	// so an in-flight job is not yanked back to `pending`
	// mid-run.
	var jobID, jobState string
	const upsertJob = `
		INSERT INTO ingest_jobs (repo_id, mode, from_sha, to_sha)
		VALUES ($1::uuid, 'delta'::ingest_mode, $2, $3)
		ON CONFLICT (repo_id, mode, (COALESCE(from_sha, '')), to_sha)
		DO UPDATE SET
			updated_at = now(),
			status     = CASE
			    WHEN ingest_jobs.status = 'failed'::ingest_status
			        THEN 'pending'::ingest_status
			    ELSE ingest_jobs.status
			END
		RETURNING job_id::text, status::text
	`
	if err := tx.QueryRowContext(ctx, upsertJob,
		repoID, fromSHA, p.ToSHA,
	).Scan(&jobID, &jobState); err != nil {
		return Response{}, fmt.Errorf("upsert ingest_jobs: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Response{}, fmt.Errorf("commit tx: %w", err)
	}

	return Response{
		EventID:  eventID,
		JobID:    jobID,
		JobState: jobState,
	}, nil
}

// reject writes the 401 response shape every unauthenticated
// path produces. Body is the literal `unauthorized` string --
// no JSON, no operator hints; the structured log carries the
// diagnostic detail.
func (h *Handler) reject(w http.ResponseWriter, _ string) {
	w.Header().Set("WWW-Authenticate", `Signature realm="agent-memory webhook"`)
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
