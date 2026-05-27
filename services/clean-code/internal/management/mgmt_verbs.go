package management

// Stage 3.4 -- mgmt.retract_sample + mgmt.rescan HTTP verbs.
//
// This file owns the Management surface's write verbs that
// reach into the Measurement sub-store via the Metric Ingestor.
//
// # Architectural invariant (architecture Sec 6.3)
//
// "Mgmt surface never writes Measurement rows directly -- it
//  only emits `repo_event` and delegates."
//
// The Management surface therefore:
//
//   * Appends `repo_event(kind='retract_intent',
//     payload={sample_id, reason})` rows for retract calls --
//     the Management role has `GRANT INSERT ON repo_event` per
//     migration 0004_roles.up.sql:313.
//
//   * DELEGATES the metric_retraction + scan_run(kind='retract')
//     writes to [metric_ingestor.RetractDispatcher] via the
//     narrow [RetractDispatcher] interface defined here. The
//     dispatcher owns the Measurement-sub-store INSERTs;
//     Management is purely the orchestration / repo_event
//     audit-trail layer.
//
//   * DELEGATES the scan_run(kind='full') open for rescan to
//     [metric_ingestor.RescanEnqueuer] via the narrow
//     [RescanEnqueuer] interface. NO repo_event is appended
//     for rescan -- the canonical RepoEvent.kind enum at
//     architecture Sec 5.1.4 line 883 has FOUR values only
//     (`registered`, `retired`, `retract_intent`,
//     `mode_changed`); no `rescan_intent`. The rescan verb
//     is a SERVICE-INTERNAL request whose audit trail lives
//     in the scan_run row itself and the structured log.
//
// # Why narrow local interfaces
//
// The [RetractDispatcher] / [RescanEnqueuer] / [SampleResolver]
// / [RepoEventAppender] interfaces are defined HERE (not
// imported from the metric_ingestor package) so:
//
//   * Tests can inject pure-Go fakes without dragging the
//     entire metric_ingestor wiring tree into the management
//     package's test deps.
//
//   * The composition root retains a free hand to switch
//     implementations (e.g. a future PG-backed
//     RepoEventAppender or a queue-backed dispatcher) without
//     touching the wire layer.
//
// Production wiring passes a `*metric_ingestor.RetractDispatcher`
// /  `*metric_ingestor.RescanEnqueuer` as the concrete value
// (they satisfy the interfaces by duck-typing).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
)

// Canonical HTTP paths for the Stage 3.4 management write
// verbs. Pinned here as exported constants so dashboards,
// runbooks, and the integration-test harness can reference
// them without re-typing the path string. The URL pattern
// mirrors the canonical verb name verbatim (dot -> slash).
const (
	// VerbMgmtRetractSamplePath mounts `mgmt.retract_sample`.
	VerbMgmtRetractSamplePath = "/v1/mgmt/retract_sample"
	// VerbMgmtRescanPath mounts `mgmt.rescan`.
	VerbMgmtRescanPath = "/v1/mgmt/rescan"
)

// Canonical repo_event kind literal for the retract intent
// log row. Pinned as a string constant rather than an
// imported enum value so a grep for `"retract_intent"`
// reveals every reference (DB enum label, architecture
// text, tech-spec invariant). The four canonical values
// per architecture Sec 5.1.4 line 883 are
// (`registered`, `retired`, `retract_intent`, `mode_changed`).
const RepoEventKindRetractIntent = "retract_intent"

// actorPrefix is stamped on `metric_retraction.appended_by`
// and the rescan structured-log attribution so a future
// reader can `LIKE 'operator:%'` to filter operator-driven
// rows. Mirrors architecture Sec 5.2.2 line 1033
// ("`operator:<oidc-subject>`").
const actorPrefix = "operator:"

// Sentinel errors emitted by the Management-side verb
// handlers.
var (
	// ErrMgmtUnknownSample is returned when the SampleResolver
	// has no record of the sample_id the caller asked to
	// retract. Mapped to 404 by the HTTP layer.
	ErrMgmtUnknownSample = errors.New("management: sample_id not found")

	// ErrMgmtRetractZeroSampleID is returned when the wire
	// body's `sample_id` is the zero UUID (the
	// JSON-deserialised form of `"00000000-..."` or an
	// omitted field).
	ErrMgmtRetractZeroSampleID = errors.New("management: retract_sample.sample_id is the zero UUID")

	// ErrMgmtRetractEmptyReason is returned when
	// `retract_sample.reason` is empty or whitespace-only.
	ErrMgmtRetractEmptyReason = errors.New("management: retract_sample.reason is empty")

	// ErrMgmtRescanZeroRepoID is returned when the wire body's
	// `repo_id` is the zero UUID.
	ErrMgmtRescanZeroRepoID = errors.New("management: rescan.repo_id is the zero UUID")

	// ErrMgmtRescanEmptySHA is returned when the wire body's
	// `sha` is empty or whitespace-only.
	ErrMgmtRescanEmptySHA = errors.New("management: rescan.sha is empty")
)

// SampleResolver is the read-side seam the retract verb uses
// to resolve `(sample_id) -> (repo_id, sha)` before appending
// the `repo_event(kind='retract_intent')` row. The Management
// surface needs `repo_id` to stamp the FK on the repo_event
// row (and the dispatcher will resolve it again on its own
// flow; the duplicate lookup is cheap and keeps each layer's
// dependency narrow).
//
// Satisfied in production by
// `*metric_ingestor.InMemoryRetractStore` (and the
// PG-backed equivalent in a follow-up stage).
type SampleResolver interface {
	// ResolveSample returns the (repo_id, sha) tuple for
	// the named sample. Returns (zero, zero, false, nil)
	// when no such sample exists. Infrastructure failure
	// returns (zero, zero, false, err).
	ResolveSample(ctx context.Context, sampleID uuid.UUID) (repoID uuid.UUID, sha string, found bool, err error)
}

// RetractionRow mirrors the Measurement-sub-store
// `metric_retraction` row shape. Defined HERE (not imported
// from metric_ingestor) so the wire surface owns its own
// vocabulary; the concrete `metric_ingestor.RetractionRow`
// is structurally equivalent.
type RetractionRow struct {
	RetractionID uuid.UUID
	SampleID     uuid.UUID
	Reason       string
	AppendedBy   string
	CreatedAt    time.Time
}

// RetractResult mirrors `metric_ingestor.RetractResult` --
// see that type's doc for the field semantics. Decoupled here
// so the wire layer doesn't import the dispatcher's package
// just for its return shape.
type RetractResult struct {
	Retraction RetractionRow
	ScanRunID  uuid.UUID
	Inserted   bool
}

// RetractDispatcher is the narrow interface the Management
// surface uses to delegate the metric_retraction +
// scan_run(kind='retract') append to the Metric Ingestor.
// Satisfied in production by
// `*metric_ingestor.RetractDispatcher`.
type RetractDispatcher interface {
	// Dispatch executes the dispatcher-side retract flow.
	// The Management surface calls this AFTER appending the
	// `repo_event(kind='retract_intent')` audit row. The
	// dispatcher returns the persisted (or already-existing,
	// per idempotency) retraction row, the scan_run id it
	// opened, and `Inserted=true` iff a new
	// metric_retraction row was written.
	Dispatch(ctx context.Context, sampleID uuid.UUID, reason, appendedBy string) (RetractResult, error)
}

// RescanResult mirrors `metric_ingestor.RescanResult` so the
// wire surface owns the response shape independently of the
// metric_ingestor type.
type RescanResult struct {
	ScanRunID   uuid.UUID
	RepoID      uuid.UUID
	SHA         string
	RequestedBy string
	OpenedAt    time.Time
}

// RescanEnqueuer is the narrow interface the Management
// surface uses to delegate the
// `scan_run(kind='full', sha_binding='single')` open to the
// Metric Ingestor. Satisfied in production by
// `*metric_ingestor.RescanEnqueuer`.
type RescanEnqueuer interface {
	Enqueue(ctx context.Context, repoID uuid.UUID, sha, requestedBy string) (RescanResult, error)
}

// RepoEventAppender is the persistence seam the Management
// surface writes the `repo_event(kind=$kind, payload=$payload)`
// row through. The Management role has column-level INSERT
// on `clean_code.repo_event` per migration
// 0004_roles.up.sql:313 -- this interface is the application
// boundary that mirrors the SQL GRANT.
//
// `payload` is rendered to the `payload_json` JSONB column;
// callers MUST pass a value json.Marshal accepts. Empty
// `payload` is permitted (the column DEFAULTs to '{}'::jsonb).
type RepoEventAppender interface {
	AppendRepoEvent(ctx context.Context, repoID uuid.UUID, kind string, payload map[string]any) error
}

// MgmtWriter is the HTTP write-side surface for the Stage 3.4
// management verbs. Wraps the four narrow dependencies and
// serves `mgmt.retract_sample` + `mgmt.rescan`.
//
// Stage 6.2 adds the optional `repoStore RepoStore` seam used
// by `mgmt.register_repo` and `mgmt.set_mode`. The seam is
// optional so an existing scaffold-mode bring-up that only
// wires retract / rescan keeps working unchanged; the new
// verbs return 503 when the store is unset.
//
// Construct via [NewMgmtWriter]. Any dependency MAY be nil for
// scaffold-mode bring-ups; the wire layer then returns 503 on
// the affected verb (mirrors the
// "verb exists, backing subsystem is down" contract pinned by
// the Stage 5.1 keys handler).
type MgmtWriter struct {
	resolver   SampleResolver
	dispatcher RetractDispatcher
	enqueuer   RescanEnqueuer
	appender   RepoEventAppender
	repoStore  RepoStore
	clock      func() time.Time
	logger     *slog.Logger
}

// MgmtWriterOption configures a [MgmtWriter] at construction.
type MgmtWriterOption func(*MgmtWriter)

// WithMgmtWriterLogger overrides the structured logger. nil
// disables logging on the handler's success and failure paths.
func WithMgmtWriterLogger(log *slog.Logger) MgmtWriterOption {
	return func(w *MgmtWriter) { w.logger = log }
}

// WithMgmtWriterClock overrides the clock used to stamp
// observability fields (it does NOT govern dispatcher- or
// enqueuer-stamped timestamps; those layers carry their own
// clock injector). Default [time.Now].
func WithMgmtWriterClock(now func() time.Time) MgmtWriterOption {
	return func(w *MgmtWriter) { w.clock = now }
}

// WithMgmtWriterRepoStore wires the [RepoStore] dependency
// used by the Stage 6.2 `mgmt.register_repo` and
// `mgmt.set_mode` verbs. nil leaves the verbs unwired (they
// return 503). Production wiring passes a real
// [InMemoryRepoStore] (during early bring-up) or the
// PG-backed implementation that lands in a follow-up stage.
//
// The option is defined here (not as a constructor argument)
// so existing callers wiring only the Stage 3.4 retract /
// rescan verbs continue to compile unchanged -- the
// management package's public NewMgmtWriter signature is
// load-bearing across the metric-ingestor and
// composition-root callers.
func WithMgmtWriterRepoStore(store RepoStore) MgmtWriterOption {
	return func(w *MgmtWriter) { w.repoStore = store }
}

// NewMgmtWriter constructs an [MgmtWriter]. Any nil argument
// disables ONLY the verb that depends on it (the affected
// verb returns 503). Production wiring passes all four
// dependencies; scaffold-mode bring-ups MAY pass nil.
func NewMgmtWriter(resolver SampleResolver, dispatcher RetractDispatcher, enqueuer RescanEnqueuer, appender RepoEventAppender, opts ...MgmtWriterOption) *MgmtWriter {
	w := &MgmtWriter{
		resolver:   resolver,
		dispatcher: dispatcher,
		enqueuer:   enqueuer,
		appender:   appender,
		clock:      time.Now,
	}
	for _, opt := range opts {
		opt(w)
	}
	if w.clock == nil {
		w.clock = time.Now
	}
	return w
}

// --- wire shapes -----------------------------------------------------------

// retractWireRequest is the inbound wire shape for
// `mgmt.retract_sample`. Mirrors the brief verbatim:
// `(sample_id, reason)`. `actor` is sourced from the
// `X-OIDC-Subject` header, NOT the body, so a caller cannot
// spoof attribution -- mirrors the trust boundary the
// Stage 5.3 override verb pins.
type retractWireRequest struct {
	SampleID string `json:"sample_id"`
	Reason   string `json:"reason"`
}

// retractWireResponse is the wire shape returned by
// [MgmtWriter.RetractSample] on success.
type retractWireResponse struct {
	RetractionID string    `json:"retraction_id"`
	SampleID     string    `json:"sample_id"`
	Reason       string    `json:"reason"`
	AppendedBy   string    `json:"appended_by"`
	CreatedAt    time.Time `json:"created_at"`
	ScanRunID    string    `json:"scan_run_id"`
	Inserted     bool      `json:"inserted"`
}

// rescanWireRequest is the inbound wire shape for
// `mgmt.rescan`. Mirrors the brief verbatim:
// `(repo_id, sha)`. `actor` is sourced from the
// `X-OIDC-Subject` header.
type rescanWireRequest struct {
	RepoID string `json:"repo_id"`
	SHA    string `json:"sha"`
}

// rescanWireResponse is the wire shape returned by
// [MgmtWriter.Rescan] on success.
type rescanWireResponse struct {
	ScanRunID   string    `json:"scan_run_id"`
	RepoID      string    `json:"repo_id"`
	SHA         string    `json:"sha"`
	RequestedBy string    `json:"requested_by"`
	OpenedAt    time.Time `json:"opened_at"`
}

// --- handlers --------------------------------------------------------------

// RetractSample serves `POST /v1/mgmt/retract_sample`.
//
// The handler's contract verbatim mirrors the workstream
// brief:
//
//  1. Append a `repo_event(kind='retract_intent',
//     payload={sample_id, reason})` row.
//  2. Dispatch to the Metric Ingestor's RetractDispatcher.
//  3. Return the resulting retraction row.
//
// Sequencing: the handler first VALIDATES the body, then
// RESOLVES sample_id -> (repo_id, sha). If the sample is
// unknown, 404 is returned BEFORE any repo_event is
// appended -- a retract_intent log row for a non-existent
// sample would be misleading audit noise. Once the sample
// is resolved the handler APPENDS the repo_event AND THEN
// dispatches; if the dispatch fails the repo_event row
// remains as a record of the operator's intent (the
// dispatch is idempotent, so a retry creates exactly one
// metric_retraction even though it produces a second
// repo_event row -- the architecturally-pinned
// append-only audit log accepts retry duplicates).
//
// Status codes:
//
//   - 200: retraction landed; body is [retractWireResponse].
//   - 400: malformed JSON, unknown body fields, or shape
//     validation failure.
//   - 401: missing or empty `X-OIDC-Subject` header.
//   - 404: sample_id does not exist
//     ([ErrMgmtUnknownSample]).
//   - 405: method other than POST.
//   - 503: any of (resolver, dispatcher, repo-event appender)
//     not wired.
//   - 500: any other internal error; opaque body.
func (w *MgmtWriter) RetractSample(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	if w.resolver == nil || w.dispatcher == nil || w.appender == nil {
		http.Error(rw, "retract surface not wired", http.StatusServiceUnavailable)
		return
	}
	actor := strings.TrimSpace(r.Header.Get(OIDCSubjectHeader))
	if actor == "" {
		http.Error(rw, fmt.Sprintf("missing or empty %s header (the OIDC subject is required)", OIDCSubjectHeader), http.StatusUnauthorized)
		return
	}
	var wire retractWireRequest
	if !decodeStrict(rw, r, &wire) {
		return
	}

	sampleID, err := uuid.FromString(wire.SampleID)
	if err != nil || sampleID == uuid.Nil {
		// Distinguish "you sent garbage" from "you sent the
		// zero UUID" so a future operator debugging a
		// payload sees the right sentinel echoed back.
		if err != nil {
			http.Error(rw, fmt.Sprintf("invalid sample_id: %s", err.Error()), http.StatusBadRequest)
			return
		}
		http.Error(rw, ErrMgmtRetractZeroSampleID.Error(), http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(wire.Reason)
	if reason == "" {
		http.Error(rw, ErrMgmtRetractEmptyReason.Error(), http.StatusBadRequest)
		return
	}

	// 1. Resolve sample_id -> (repo_id, sha). Bail BEFORE
	// any repo_event when the sample doesn't exist.
	repoID, _, found, err := w.resolver.ResolveSample(r.Context(), sampleID)
	if err != nil {
		w.logError(r.Context(), "mgmt.retract_sample", "resolver", err)
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.Error(rw, fmt.Sprintf("%s: %s", ErrMgmtUnknownSample.Error(), sampleID), http.StatusNotFound)
		return
	}

	// 2. Append `repo_event(kind='retract_intent', payload={sample_id, reason})`.
	// This is the operator-intent audit row -- it lands
	// BEFORE the dispatcher's Measurement-sub-store writes
	// so an operator-intent record survives even if
	// dispatch fails.
	//
	// payload key set ("sample_id", "reason") is pinned by
	// the workstream brief verbatim.
	payload := map[string]any{
		"sample_id": sampleID.String(),
		"reason":    reason,
	}
	if err := w.appender.AppendRepoEvent(r.Context(), repoID, RepoEventKindRetractIntent, payload); err != nil {
		w.logError(r.Context(), "mgmt.retract_sample", "append_repo_event", err)
		http.Error(rw, "internal error", http.StatusInternalServerError)
		return
	}

	// 3. Dispatch to the Metric Ingestor's RetractDispatcher.
	// `appended_by` is stamped `operator:<subject>` per
	// architecture Sec 5.2.2 line 1033.
	appendedBy := actorPrefix + actor
	res, err := w.dispatcher.Dispatch(r.Context(), sampleID, reason, appendedBy)
	if err != nil {
		writeRetractDispatchError(rw, r, err, w.logger)
		return
	}

	if w.logger != nil {
		w.logger.InfoContext(r.Context(), "mgmt.retract_sample succeeded",
			"verb", "mgmt.retract_sample",
			"sample_id", sampleID.String(),
			"repo_id", repoID.String(),
			"scan_run_id", res.ScanRunID.String(),
			"retraction_id", res.Retraction.RetractionID.String(),
			"inserted", res.Inserted,
			"actor", actor,
		)
	}

	writeJSON(rw, r, "mgmt.retract_sample", http.StatusOK, retractWireResponse{
		RetractionID: res.Retraction.RetractionID.String(),
		SampleID:     res.Retraction.SampleID.String(),
		Reason:       res.Retraction.Reason,
		AppendedBy:   res.Retraction.AppendedBy,
		CreatedAt:    res.Retraction.CreatedAt.UTC(),
		ScanRunID:    res.ScanRunID.String(),
		Inserted:     res.Inserted,
	})
}

// Rescan serves `POST /v1/mgmt/rescan`.
//
// The handler's contract verbatim mirrors the workstream
// brief:
//
//  1. NO `repo_event` is appended -- no canonical
//     RepoEvent.kind value exists for rescan per architecture
//     Sec 5.1.4.
//  2. Delegate to the Metric Ingestor's
//     [RescanEnqueuer.Enqueue] which opens a
//     `scan_run(kind='full', sha_binding='single',
//     status='running', to_sha=<sha>)` row.
//
// Status codes:
//
//   - 200: scan_run opened; body is [rescanWireResponse].
//   - 400: malformed JSON, unknown body fields, or shape
//     validation failure.
//   - 401: missing or empty `X-OIDC-Subject` header.
//   - 405: method other than POST.
//   - 503: enqueuer not wired.
//   - 500: any other internal error; opaque body.
func (w *MgmtWriter) Rescan(rw http.ResponseWriter, r *http.Request) {
	if !requirePOST(rw, r) {
		return
	}
	if w.enqueuer == nil {
		http.Error(rw, "rescan surface not wired", http.StatusServiceUnavailable)
		return
	}
	actor := strings.TrimSpace(r.Header.Get(OIDCSubjectHeader))
	if actor == "" {
		http.Error(rw, fmt.Sprintf("missing or empty %s header (the OIDC subject is required)", OIDCSubjectHeader), http.StatusUnauthorized)
		return
	}
	var wire rescanWireRequest
	if !decodeStrict(rw, r, &wire) {
		return
	}

	repoID, err := uuid.FromString(wire.RepoID)
	if err != nil || repoID == uuid.Nil {
		if err != nil {
			http.Error(rw, fmt.Sprintf("invalid repo_id: %s", err.Error()), http.StatusBadRequest)
			return
		}
		http.Error(rw, ErrMgmtRescanZeroRepoID.Error(), http.StatusBadRequest)
		return
	}
	sha := strings.TrimSpace(wire.SHA)
	if sha == "" {
		http.Error(rw, ErrMgmtRescanEmptySHA.Error(), http.StatusBadRequest)
		return
	}

	requestedBy := actorPrefix + actor
	res, err := w.enqueuer.Enqueue(r.Context(), repoID, sha, requestedBy)
	if err != nil {
		writeRescanEnqueueError(rw, r, err, w.logger)
		return
	}

	if w.logger != nil {
		w.logger.InfoContext(r.Context(), "mgmt.rescan succeeded",
			"verb", "mgmt.rescan",
			"repo_id", repoID.String(),
			"sha", sha,
			"scan_run_id", res.ScanRunID.String(),
			"actor", actor,
		)
	}

	writeJSON(rw, r, "mgmt.rescan", http.StatusOK, rescanWireResponse{
		ScanRunID:   res.ScanRunID.String(),
		RepoID:      res.RepoID.String(),
		SHA:         res.SHA,
		RequestedBy: res.RequestedBy,
		OpenedAt:    res.OpenedAt.UTC(),
	})
}

// Routes returns an `http.ServeMux` ready to mount onto the
// service's HTTP listener with the Stage 3.4 + Stage 6.2 verbs
// at their canonical paths.
//
// Mounted unconditionally:
//   - `mgmt.retract_sample` (Stage 3.4)
//   - `mgmt.rescan`         (Stage 3.4)
//
// Mounted only when [WithMgmtWriterRepoStore] supplied a
// non-nil store (otherwise the routes 503 anyway, so the
// extra mount adds no value):
//   - `mgmt.register_repo`  (Stage 6.2)
//   - `mgmt.set_mode`       (Stage 6.2)
//
// For a UNIFIED management surface that ALSO mounts
// `mgmt.override` (which lives on [PolicyWriter]), use
// [MgmtSurfaceRoutes] -- this method is the writer-side
// subset and intentionally does NOT mount override (a
// PolicyWriter is not in scope here).
func (w *MgmtWriter) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(VerbMgmtRetractSamplePath, w.RetractSample)
	mux.HandleFunc(VerbMgmtRescanPath, w.Rescan)
	if w.repoStore != nil {
		mux.HandleFunc(VerbMgmtRegisterRepoPath, w.RegisterRepo)
		mux.HandleFunc(VerbMgmtSetModePath, w.SetMode)
	}
	return mux
}

// --- error translation -----------------------------------------------------

// writeRetractDispatchError maps a dispatcher-side error to
// an HTTP status. Sentinel mapping:
//
//   - errors.Is matches [ErrMgmtUnknownSample]          -> 404
//     (a management-layer "this sample never existed" path)
//   - errors.Is matches
//     [metric_ingestor.ErrRetractUnknownSample]         -> 404
//     (the dispatcher's sample-vanished-between-resolve-
//     and-dispatch race; the dispatcher wraps the sentinel
//     with `%w` per retract.go, so the chain walks cleanly)
//   - anything else                                      -> 500
//
// Both checks are sentinel-based (errors.Is), NOT substring,
// so reworded error messages on either side do not silently
// degrade the mapping to 500. The management package already
// imports metric_ingestor (via mgmt_adapters.go) so referencing
// the sentinel here costs no new package dependency.
func writeRetractDispatchError(rw http.ResponseWriter, r *http.Request, err error, log *slog.Logger) {
	if errors.Is(err, ErrMgmtUnknownSample) || errors.Is(err, metric_ingestor.ErrRetractUnknownSample) {
		http.Error(rw, err.Error(), http.StatusNotFound)
		return
	}
	if log != nil {
		log.ErrorContext(r.Context(), "mgmt.retract_sample dispatcher failed",
			"verb", "mgmt.retract_sample",
			"err", err.Error(),
		)
	}
	http.Error(rw, "internal error", http.StatusInternalServerError)
}

// writeRescanEnqueueError maps an enqueuer-side error to an
// HTTP status. Stage 3.4 only surfaces 500 here -- there is
// no canonical not-found mapping for an unknown repo at the
// enqueuer layer (the PG FK on `scan_run.repo_id ->
// repo.repo_id` enforces this server-side and surfaces as a
// raw driver error, which is opaque-mapped to 500). A
// future stage MAY introduce an explicit `ErrUnknownRepo`
// sentinel; the wire-layer mapping lives here so that
// uplift is a one-file change.
func writeRescanEnqueueError(rw http.ResponseWriter, r *http.Request, err error, log *slog.Logger) {
	if log != nil {
		log.ErrorContext(r.Context(), "mgmt.rescan enqueuer failed",
			"verb", "mgmt.rescan",
			"err", err.Error(),
		)
	}
	http.Error(rw, "internal error", http.StatusInternalServerError)
}

// logError writes a structured error log for the named verb
// and step. Centralised so every internal-error path emits
// the same shape -- operators can grep for
// `"verb":"mgmt.retract_sample"` to find every failure
// regardless of which step inside the handler tripped.
func (w *MgmtWriter) logError(ctx context.Context, verb, step string, err error) {
	if w.logger == nil {
		return
	}
	w.logger.ErrorContext(ctx, "management write verb failed",
		"verb", verb,
		"step", step,
		"err", err.Error(),
	)
}

// --- in-memory RepoEventAppender (Stage 3.4 scaffold) ---------------------

// InMemoryRepoEventAppender is an in-process implementation of
// [RepoEventAppender] suitable for unit tests and the early
// composition root (before the PG-backed appender lands in a
// follow-up stage). The store carries the persisted rows in
// append order; tests inspect them via [Events] /
// [EventsForRepo].
//
// Concurrent calls are serialised by an internal mutex so
// parallel HTTP handler tests can share one store.
type InMemoryRepoEventAppender struct {
	mu     sync.Mutex
	events []RepoEventRow
}

// RepoEventRow is the in-memory shape of one persisted
// `clean_code.repo_event` row.
type RepoEventRow struct {
	EventID   uuid.UUID
	RepoID    uuid.UUID
	Kind      string
	Payload   map[string]any
	CreatedAt time.Time
}

// NewInMemoryRepoEventAppender returns a fresh appender with
// no seeded rows.
func NewInMemoryRepoEventAppender() *InMemoryRepoEventAppender {
	return &InMemoryRepoEventAppender{}
}

// AppendRepoEvent implements [RepoEventAppender]. Mints a
// fresh event_id, deep-copies the payload (callers MAY hand
// in a map they later mutate), and timestamps with
// [time.Now]. Returns an error on `uuid.NewV4` failure.
func (a *InMemoryRepoEventAppender) AppendRepoEvent(ctx context.Context, repoID uuid.UUID, kind string, payload map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if repoID == uuid.Nil {
		return errors.New("management: InMemoryRepoEventAppender: repoID is the zero UUID")
	}
	if strings.TrimSpace(kind) == "" {
		return errors.New("management: InMemoryRepoEventAppender: kind is empty")
	}
	id, err := uuid.NewV4()
	if err != nil {
		return fmt.Errorf("management: mint event_id: %w", err)
	}
	// Defensive copy of payload so a caller mutating the map
	// post-AppendRepoEvent doesn't change the persisted row.
	copied := make(map[string]any, len(payload))
	for k, v := range payload {
		copied[k] = v
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, RepoEventRow{
		EventID:   id,
		RepoID:    repoID,
		Kind:      kind,
		Payload:   copied,
		CreatedAt: time.Now().UTC(),
	})
	return nil
}

// Events returns a snapshot of every appended row in append
// order.
func (a *InMemoryRepoEventAppender) Events() []RepoEventRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]RepoEventRow, len(a.events))
	copy(out, a.events)
	return out
}

// EventsForRepo returns the subset of appended rows whose
// `RepoID` matches. Convenience helper for tests asserting
// per-repo invariants.
func (a *InMemoryRepoEventAppender) EventsForRepo(repoID uuid.UUID) []RepoEventRow {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]RepoEventRow, 0, len(a.events))
	for _, e := range a.events {
		if e.RepoID == repoID {
			out = append(out, e)
		}
	}
	return out
}

// Count returns the number of appended rows.
func (a *InMemoryRepoEventAppender) Count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.events)
}

// Compile-time interface guard.
var _ RepoEventAppender = (*InMemoryRepoEventAppender)(nil)

// Suppress unused-import warning for json.Decoder when this
// file is built with no test file in the package. The
// decoder helper lives in policy_verbs.go.
var _ = json.NewDecoder
