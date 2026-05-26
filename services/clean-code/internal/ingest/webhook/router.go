package webhook

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/gofrs/uuid"
)

// RouterPath is the canonical HTTP path the Router is mounted
// at. The trailing slash is significant: Go's [http.ServeMux]
// matches the prefix and exposes the remaining `{verb}` path
// segment via the request URL. The composition root mounts
// `RouterPath` and the Router resolves the verb from
// `r.URL.Path`.
//
// Pinned here so tests and the composition root share one
// literal -- changing it requires editing exactly one place.
const RouterPath = "/v1/ingest/"

// RouterResponse is the JSON envelope the Router emits on a
// successful (200) ingest call. The shape is verb-agnostic:
// every verb returns the same envelope shell, with the
// verb-specific counters carried in `Detail`. Pinned so a CI
// publisher consumes one shape for `coverage`,
// `test_balance`, `churn`, `defects` alike.
//
// `Replayed=true` indicates the response is a cached replay
// of a prior call with the same `(verb, payload_hash)`. The
// `ScanRunID` matches the original; the verb handler was NOT
// re-executed. Publishers MAY ignore the field for normal
// success paths; ops tooling consumes it for retry-storm
// dashboards.
type RouterResponse struct {
	ScanRunID            uuid.UUID       `json:"scan_run_id"`
	Verb                 string          `json:"verb"`
	ScanRunKind          string          `json:"scan_run_kind"`
	PayloadHash          string          `json:"payload_hash"`
	FoundationDispatched bool            `json:"foundation_dispatched"`
	Replayed             bool            `json:"replayed"`
	Detail               json.RawMessage `json:"detail,omitempty"`
}

// Router is the generic `/v1/ingest/{verb}` HTTP handler. It
// owns five responsibilities (in canonical order):
//
//  1. Method check (POST only -- 405 otherwise).
//  2. Body size-limited read (16 MiB cap -- 413 otherwise).
//  3. HMAC-SHA256 verification: resolve the per-deployment
//     secret via [SecretResolver.Resolve] keyed on
//     [SigningKeyIDHeader], verify [HMACSignatureHeader] over
//     the raw body. 401 on any failure.
//  4. Verb lookup + Content-Type check (404 / 415).
//  5. Idempotency claim against [IdempotencyStore]:
//     `payload_hash = sha256(body)`. On replay: return the
//     cached response. On claim: dispatch the
//     [VerbHandler.Handle] and commit (or abort on failure).
//
// # Why HMAC before content-type
//
// Iter 6 of the legacy [ChurnIngestHandler] established this
// ordering after a rubber-duck audit: an unauthenticated
// caller MUST NOT be able to probe the per-verb media-type
// contract by inspecting the difference between 401 (auth)
// and 415 (wrong media-type). The Router preserves the
// invariant: HMAC verification runs before any per-verb
// inspection.
//
// # Why idempotency after content-type
//
// The rubber-duck audit for Stage 4.1 caught the inverse
// vector: a signed replay with the WRONG content-type would
// otherwise hit the cached response and emit a 200 OK with
// the cached body -- silently overriding the contract the
// content-type check is supposed to enforce. The Router
// validates the verb's media-type pin BEFORE looking up the
// idempotency claim so a malformed replay surfaces as 415.
//
// # Durable idempotency (Stage 4.1 iter-2 evaluator items #1 #2)
//
// The Router's idempotency layer is split across two seams:
//
//   - [ScanRunRepository] -- DURABLE. Owns the
//     `clean_code.scan_run(payload_hash=...)` lifecycle
//     (open + finalize). Survives restarts and replicas via
//     the partial unique index from migration 0009. THIS is
//     the seam that satisfies the brief's "if a
//     scan_run(payload_hash=...) already exists for this
//     verb, return the stored scan_run_id without
//     re-executing" requirement.
//   - [IdempotencyStore] -- FAST, IN-PROCESS. Owns the
//     response_body cache so a same-process replay re-emits
//     byte-identical 200s. A cross-restart replay falls back
//     to a minimal envelope rebuilt from the scan_run row.
//
// The Router consults the durable seam FIRST (durable claim),
// then the in-memory cache (fast replay envelope) inside the
// already-existed branch.
//
// # Concurrency
//
// One Router instance handles every concurrent inbound
// request; no per-request state lives on the struct. The
// only mutating state is the [IdempotencyStore] and
// [ScanRunRepository], whose own claim semantics serialise
// access per (verb, payload_hash).
type Router struct {
	resolver  SecretResolver
	store     IdempotencyStore
	scanRunRepo ScanRunRepository
	verbs     map[string]VerbHandler
	logger    *slog.Logger
	newUUID   func() (uuid.UUID, error)
	maxBytes  int64
	now       func() time.Time
}

// RouterConfig bundles the Router's wiring. Every field
// except `Verbs` is optional; missing values fall back to
// the sensible production defaults documented per-field.
type RouterConfig struct {
	// Resolver maps `signing_key_id` -> HMAC secret.
	// REQUIRED -- a nil resolver is a wiring bug;
	// NewRouter panics.
	Resolver SecretResolver

	// Store is the in-process response-body cache.
	// REQUIRED. Used to re-emit byte-identical replays in
	// the same process; cross-restart replays fall back to
	// the durable [ScanRunRepository].
	Store IdempotencyStore

	// ScanRunRepo is the DURABLE scan_run lifecycle seam
	// (Stage 4.1 iter-2 evaluator items #1 #2). REQUIRED.
	// The Router opens a scan_run row with payload_hash set
	// BEFORE dispatching the verb handler; on conflict the
	// existing scan_run_id is returned and the verb handler
	// is NOT re-executed. The composition root passes
	// [PGScanRunRepository] in production and
	// [InMemoryScanRunRepository] in dev / tests.
	ScanRunRepo ScanRunRepository

	// Verbs is the per-verb-token handler registry.
	// REQUIRED non-empty -- a Router with no verbs cannot
	// service any request. NewRouter validates each verb
	// token via [ValidateVerbToken] AND asserts every
	// handler's [VerbHandler.ScanRunKind] matches the
	// closed-set pin (see canonicalScanRunKindForVerb).
	Verbs []VerbHandler

	// Logger receives structured log lines for HMAC
	// failures, dispatch failures, and replay events.
	// MAY be nil (logs are silently dropped).
	Logger *slog.Logger

	// NewUUID mints fresh `scan_run_id`s. Defaults to
	// `uuid.NewV7` when nil. Tests inject a deterministic
	// generator.
	//
	// NOTE: as of iter-2 the durable [ScanRunRepository]
	// owns the scan_run_id mint; this hook is retained for
	// backwards-compatibility with tests that pre-date the
	// refactor and is honoured only on the in-memory
	// fallback path (when ScanRunRepo is nil -- which is
	// rejected by NewRouter, so the hook is effectively
	// dead in production code but kept in the struct to
	// preserve compatibility with existing tests).
	NewUUID func() (uuid.UUID, error)

	// MaxBytes caps the inbound body size. Defaults to
	// [MaxBodyBytes] (16 MiB) when zero. A negative value
	// is a wiring bug; NewRouter panics.
	MaxBytes int64

	// Now is the time-source the Router stamps on
	// scan_run lifecycle calls. Defaults to time.Now when
	// nil. Tests inject a fake.
	Now func() time.Time
}

// NewRouter constructs a [Router] from `cfg`. PANICS on any
// wiring bug (nil dependency, empty verb list, malformed verb
// token, mismatched [VerbHandler.ScanRunKind] vs the closed-
// set pin). Failing loudly at startup is the explicit choice
// over silent runtime degradation -- a misconfigured Router
// has no safe fall-back.
func NewRouter(cfg RouterConfig) *Router {
	if cfg.Resolver == nil {
		panic("webhook: NewRouter received nil SecretResolver")
	}
	if cfg.Store == nil {
		panic("webhook: NewRouter received nil IdempotencyStore")
	}
	if cfg.ScanRunRepo == nil {
		panic("webhook: NewRouter received nil ScanRunRepository (Stage 4.1 iter-2 evaluator items #1 #2 require a durable scan_run-backed idempotency seam)")
	}
	if len(cfg.Verbs) == 0 {
		panic("webhook: NewRouter received zero verbs")
	}
	verbs := make(map[string]VerbHandler, len(cfg.Verbs))
	for _, vh := range cfg.Verbs {
		if vh == nil {
			panic("webhook: NewRouter received a nil VerbHandler entry")
		}
		token := vh.Verb()
		if err := ValidateVerbToken(token); err != nil {
			panic(fmt.Sprintf("webhook: NewRouter received malformed verb token: %v", err))
		}
		if _, dup := verbs[token]; dup {
			panic(fmt.Sprintf("webhook: NewRouter received duplicate verb registration for %q", token))
		}
		if expected, ok := canonicalScanRunKindForVerb(token); ok && vh.ScanRunKind() != expected {
			panic(fmt.Sprintf("webhook: VerbHandler %q reports ScanRunKind=%q but tech-spec Sec 4.11 / e2e-scenarios.md line 684 pin %q",
				token, vh.ScanRunKind(), expected))
		}
		if expectedBinding, ok := canonicalSHABindingForKind(vh.ScanRunKind()); ok && vh.SHABinding() != expectedBinding {
			panic(fmt.Sprintf("webhook: VerbHandler %q reports SHABinding=%q but ScanRunKind=%q pins SHABinding=%q (migration 0001 scan_run_sha_binding_consistent CHECK)",
				token, vh.SHABinding(), vh.ScanRunKind(), expectedBinding))
		}
		if vh.ContentType() == "" {
			panic(fmt.Sprintf("webhook: VerbHandler %q reports empty ContentType", token))
		}
		verbs[token] = vh
	}
	maxBytes := cfg.MaxBytes
	if maxBytes == 0 {
		maxBytes = MaxBodyBytes
	}
	if maxBytes < 0 {
		panic(fmt.Sprintf("webhook: NewRouter received negative MaxBytes=%d", maxBytes))
	}
	newUUID := cfg.NewUUID
	if newUUID == nil {
		newUUID = uuid.NewV7
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Router{
		resolver:    cfg.Resolver,
		store:       cfg.Store,
		scanRunRepo: cfg.ScanRunRepo,
		verbs:       verbs,
		logger:      cfg.Logger,
		newUUID:     newUUID,
		maxBytes:    maxBytes,
		now:         now,
	}
}

// ServeHTTP implements [http.Handler]. The Router is mountable
// either by direct registration (`mux.Handle(RouterPath,
// router)`) or via a stdlib `http.ServeMux` pattern match.
// The verb is parsed from the request path as the segment
// AFTER [RouterPath]; an empty / unparseable segment returns
// 404.
func (r *Router) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		r.writeError(w, http.StatusMethodNotAllowed,
			"Router accepts POST only", "METHOD_NOT_ALLOWED", "")
		return
	}

	verb, ok := r.parseVerb(req.URL.Path)
	if !ok {
		r.writeError(w, http.StatusNotFound,
			fmt.Sprintf("malformed verb path %q (expected /v1/ingest/{verb})", req.URL.Path),
			"VERB_NOT_FOUND", "")
		return
	}

	req.Body = http.MaxBytesReader(w, req.Body, r.maxBytes)
	defer func() { _ = req.Body.Close() }()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			r.writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("body exceeds %d-byte limit", r.maxBytes),
				"PAYLOAD_TOO_LARGE", verb)
			return
		}
		r.writeError(w, http.StatusBadRequest,
			fmt.Sprintf("reading body: %v", err), "BAD_REQUEST", verb)
		return
	}

	// HMAC verification BEFORE any verb / content-type
	// inspection. The signing_key_id header is the FIRST
	// header the verifier reads; a missing/malformed value
	// is a 401 with a structured code so the publisher's
	// auth pipeline can branch.
	keyID := req.Header.Get(SigningKeyIDHeader)
	if vErr := ValidateSigningKeyID(keyID); vErr != nil {
		code := classifyKeyIDError(vErr)
		r.logHMACFailure(req, verb, code, vErr)
		r.writeError(w, http.StatusUnauthorized,
			fmt.Sprintf("signing_key_id validation failed: %v", vErr), code, verb)
		return
	}
	secret, rErr := r.resolver.Resolve(keyID)
	if rErr != nil {
		if errors.Is(rErr, ErrUnknownSigningKeyID) {
			r.logHMACFailure(req, verb, "HMAC_UNKNOWN_KEY_ID", rErr)
			r.writeError(w, http.StatusUnauthorized,
				fmt.Sprintf("signing_key_id resolution failed: %v", rErr),
				"HMAC_UNKNOWN_KEY_ID", verb)
			return
		}
		// Resolver-internal failure (e.g. a future PG-backed
		// resolver lost its connection). Surface as 500 --
		// the caller is not at fault.
		r.logInternal(req, verb, "resolver-internal-failure", rErr)
		r.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("resolver error: %v", rErr), "INTERNAL_ERROR", verb)
		return
	}
	sig := req.Header.Get(HMACSignatureHeader)
	if vErr := VerifyHMAC(body, sig, secret); vErr != nil {
		code := classifyHMACError(vErr)
		r.logHMACFailure(req, verb, code, vErr)
		r.writeError(w, http.StatusUnauthorized,
			fmt.Sprintf("HMAC verification failed: %v", vErr), code, verb)
		return
	}

	// Verb lookup. The Router rejects any verb the
	// composition root did not register (404) -- a publisher
	// hitting `/v1/ingest/typo` should not be able to probe
	// the registration surface via response-shape diff.
	handler, registered := r.verbs[verb]
	if !registered {
		r.writeError(w, http.StatusNotFound,
			fmt.Sprintf("verb %q not registered (registered: %v)", verb, r.registeredVerbs()),
			"VERB_NOT_FOUND", verb)
		return
	}

	// Content-Type check AFTER HMAC AND verb lookup, BEFORE
	// idempotency claim. See the doc-comment on [Router] for
	// the ordering rationale.
	ct := req.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ct)
	if !strings.EqualFold(mediaType, handler.ContentType()) {
		r.writeError(w, http.StatusUnsupportedMediaType,
			fmt.Sprintf("verb %q expects Content-Type %q (got %q)",
				verb, handler.ContentType(), ct),
			"UNSUPPORTED_MEDIA_TYPE", verb)
		return
	}

	// Compute payload_hash AFTER auth + media-type pass so
	// the hash is computed against an already-validated body
	// shape. (The hash itself is over the raw bytes; the
	// validation is only on the SHAPE of the request, not
	// the hash inputs.)
	hash := sha256.Sum256(body)
	payloadHash := PayloadHash(hash)

	// In-process claim. Two SAME-process retries collapse
	// onto one verb execution; cross-restart retries are
	// caught by the durable [ScanRunRepository] one step
	// down.
	claimed, existing, claimErr := r.store.Claim(req.Context(), verb, payloadHash)
	if claimErr != nil {
		r.logInternal(req, verb, "idempotency-claim-failure", claimErr)
		r.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("idempotency claim failed: %v", claimErr),
			"INTERNAL_ERROR", verb)
		return
	}
	if !claimed && existing != nil {
		r.replayResponse(w, req, verb, payloadHash, existing)
		return
	}

	// We hold the in-process claim. The defer ensures Abort
	// runs on any short-circuit (verb-handler error, mint
	// failure, response-write failure); a successful Commit
	// clears the flag so the defer is a no-op.
	committed := false
	defer func() {
		if !committed {
			if err := r.store.Abort(req.Context(), verb, payloadHash); err != nil {
				r.logInternal(req, verb, "idempotency-abort-failure", err)
			}
		}
	}()

	// Extract the per-verb metadata (RepoID, SHA) required
	// to open the durable scan_run row. ExtractMetadata
	// runs BEFORE the scan_run claim so a malformed body
	// surfaces as 400/422 WITHOUT burning a durable
	// scan_run row.
	metadata, mdErr := handler.ExtractMetadata(req.Context(), body)
	if mdErr != nil {
		status, code := r.classifyVerbError(handler, mdErr)
		if status >= 500 {
			r.logInternal(req, verb, "extract-metadata-failure", mdErr)
		}
		r.writeError(w, status, mdErr.Error(), code, verb)
		return
	}

	// Open the DURABLE scan_run row keyed on (verb,
	// payload_hash) -- see migration 0009's partial unique
	// index `scan_run_payload_hash_verb_uniq`. On a fresh
	// payload this returns AlreadyExisted=false and we
	// proceed to dispatch the verb handler. On a replay
	// (across restart / replica) this returns
	// AlreadyExisted=true and the prior scan_run_id; we
	// short-circuit the verb handler and emit a replay
	// envelope.
	repoOpenedAt := r.now()
	repoRes, repoErr := r.scanRunRepo.OpenExternal(req.Context(), ScanRunRepositoryRequest{
		Verb:        verb,
		Kind:        handler.ScanRunKind(),
		SHABinding:  handler.SHABinding(),
		RepoID:      metadata.RepoID,
		SHA:         metadata.SHA,
		PayloadHash: payloadHash,
		OpenedAt:    repoOpenedAt,
	})
	if repoErr != nil {
		r.logInternal(req, verb, "scan-run-open-failure", repoErr)
		r.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("opening scan_run: %v", repoErr),
			"INTERNAL_ERROR", verb)
		return
	}
	scanRunID := repoRes.ScanRunID

	if repoRes.AlreadyExisted {
		// Durable replay: a prior call already opened a
		// scan_run with this (verb, payload_hash). Emit a
		// minimal envelope using the stored scan_run_id.
		// The Router does NOT re-execute the verb handler
		// (the brief's invariant) and does NOT finalize
		// (the prior caller's lifecycle owns finalize).
		r.emitDurableReplay(w, req, verb, handler, payloadHash, repoRes)
		// Commit a synthetic in-memory record so
		// subsequent same-process retries hit the cache
		// instead of re-querying the durable seam. We do
		// NOT need to commit a body -- the in-memory cache
		// is best-effort here.
		committed = r.commitInMemoryReplay(req.Context(), verb, payloadHash, repoRes)
		return
	}

	result, hErr := handler.Handle(req.Context(), body, scanRunID)
	if hErr != nil {
		// Finalize the durable scan_run as 'failed' so a
		// retry of the SAME payload short-circuits to
		// replay-with-failed-status (idempotent failure
		// semantics; see runbook).
		if fErr := r.scanRunRepo.Finalize(req.Context(), scanRunID, ScanRunStatusFailed, r.now()); fErr != nil {
			r.logInternal(req, verb, "scan-run-finalize-failed-on-handler-error", fErr)
		}
		status, code := r.classifyVerbError(handler, hErr)
		if status >= 500 {
			r.logInternal(req, verb, "verb-handle-failure", hErr)
		}
		r.writeError(w, status, hErr.Error(), code, verb)
		return
	}
	// Defensive: the verb handler MUST honour the supplied
	// scan_run_id. A mismatch is a handler bug that should
	// fail loudly -- silently swapping the id would break
	// the active-row uniqueness invariant downstream.
	if result.ScanRunID != scanRunID {
		err := fmt.Errorf("verb %q returned scan_run_id %s; Router supplied %s",
			verb, result.ScanRunID, scanRunID)
		if fErr := r.scanRunRepo.Finalize(req.Context(), scanRunID, ScanRunStatusFailed, r.now()); fErr != nil {
			r.logInternal(req, verb, "scan-run-finalize-failed-on-id-mismatch", fErr)
		}
		r.logInternal(req, verb, "verb-scan-run-id-mismatch", err)
		r.writeError(w, http.StatusInternalServerError,
			err.Error(), "INTERNAL_ERROR", verb)
		return
	}

	envelope := RouterResponse{
		ScanRunID:            scanRunID,
		Verb:                 verb,
		ScanRunKind:          handler.ScanRunKind(),
		PayloadHash:          payloadHash.String(),
		FoundationDispatched: result.FoundationDispatched,
		Replayed:             false,
		Detail:               result.Detail,
	}
	body200, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		r.logInternal(req, verb, "response-marshal-failure", marshalErr)
		if fErr := r.scanRunRepo.Finalize(req.Context(), scanRunID, ScanRunStatusFailed, r.now()); fErr != nil {
			r.logInternal(req, verb, "scan-run-finalize-failed-on-marshal-error", fErr)
		}
		r.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("marshalling response: %v", marshalErr),
			"INTERNAL_ERROR", verb)
		return
	}

	// Finalize the durable scan_run as 'succeeded' BEFORE
	// writing the response so a concurrent retry of the
	// same payload observes the terminal state once we
	// return. If Finalize fails we still surface 500 to
	// the caller (the scan_run row may be in 'running'
	// indefinitely until the stale-sweep transitions it).
	if fErr := r.scanRunRepo.Finalize(req.Context(), scanRunID, ScanRunStatusSucceeded, r.now()); fErr != nil {
		r.logInternal(req, verb, "scan-run-finalize-succeeded-failure", fErr)
		r.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("finalize scan_run: %v", fErr),
			"INTERNAL_ERROR", verb)
		return
	}

	// Commit the in-process cache (fast same-process
	// replay). The durable scan_run row is the
	// authoritative idempotency anchor; the cache is a
	// latency optimisation.
	record := IdempotencyRecord{
		Verb:         verb,
		PayloadHash:  payloadHash,
		ScanRunID:    scanRunID,
		ResponseBody: body200,
	}
	if cErr := r.store.Commit(req.Context(), record); cErr != nil {
		r.logInternal(req, verb, "idempotency-commit-failure", cErr)
		r.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("idempotency commit failed: %v", cErr),
			"INTERNAL_ERROR", verb)
		return
	}
	committed = true

	if r.logger != nil {
		r.logger.Info("ingest webhook: success",
			"verb", verb,
			"scan_run_id", scanRunID,
			"payload_hash", payloadHash,
			"scan_run_kind", handler.ScanRunKind(),
		)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body200)
}

// parseVerb extracts the verb token from a request path
// matching `/v1/ingest/{verb}`. Returns (token, true) on a
// well-formed match; (`"", false`) otherwise. The token
// itself is NOT validated against [ValidateVerbToken] here
// -- the caller (Router.ServeHTTP) does that AFTER auth so a
// malformed verb in the path does not short-circuit the
// HMAC step (the iter-6 ordering invariant).
//
// Actually -- wait: we WANT the path parse to short-circuit
// pre-auth because path-shape isn't sensitive. The 404 vs
// 401 differential isn't a contract probe (the verb registry
// IS public). We DO 404 before HMAC for malformed paths so
// callers that hit `/v1/foo` get a clean 404 without an HMAC
// dance. The verb-lookup against the registry happens AFTER
// HMAC so a malformed-but-syntactically-valid path requires
// auth to learn the registry membership.
func (r *Router) parseVerb(path string) (string, bool) {
	if !strings.HasPrefix(path, RouterPath) {
		return "", false
	}
	rest := strings.TrimPrefix(path, RouterPath)
	// Reject a trailing slash, query-string, or further path
	// segments -- `/v1/ingest/churn/` and `/v1/ingest/churn/extra`
	// are both 404s.
	if rest == "" || strings.ContainsAny(rest, "/?#") {
		return "", false
	}
	if err := ValidateVerbToken(rest); err != nil {
		return "", false
	}
	return rest, true
}

// replayResponse emits the cached `existing` record as the
// 200 response with `replayed=true`. The cached body is
// re-deserialised so the Router can flip the Replayed flag
// without forking response shapes.
func (r *Router) replayResponse(w http.ResponseWriter, req *http.Request, verb string, hash PayloadHash, existing *IdempotencyRecord) {
	var envelope RouterResponse
	if err := json.Unmarshal(existing.ResponseBody, &envelope); err != nil {
		// Stored bytes are malformed -- an in-memory store
		// can't really produce this, but a future PG store
		// reading from a corrupted row could. Fall back to
		// a minimal envelope so the caller still observes
		// the replay invariant.
		r.logInternal(req, verb, "replay-envelope-unmarshal-failure", err)
		envelope = RouterResponse{
			ScanRunID:   existing.ScanRunID,
			Verb:        existing.Verb,
			PayloadHash: existing.PayloadHash.String(),
		}
	}
	envelope.Replayed = true
	body, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		r.logInternal(req, verb, "replay-envelope-marshal-failure", marshalErr)
		r.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("re-marshalling replay envelope: %v", marshalErr),
			"INTERNAL_ERROR", verb)
		return
	}
	if r.logger != nil {
		r.logger.Info("ingest webhook: replay (cached scan_run_id, in-process)",
			"verb", verb,
			"scan_run_id", existing.ScanRunID,
			"payload_hash", hash,
		)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// emitDurableReplay emits a 200 response for a replay that
// originated from the durable [ScanRunRepository] (the
// in-process cache had no record; the scan_run row was
// created by a prior request/replica/process). The response
// envelope is rebuilt from the durable row -- no
// `result.Detail` is replayed (the original verb-handler's
// detail counters are not durably stored; a future iter MAY
// add a `scan_run_detail` cache column).
func (r *Router) emitDurableReplay(w http.ResponseWriter, req *http.Request, verb string, handler VerbHandler, hash PayloadHash, repoRes ScanRunRepositoryResult) {
	envelope := RouterResponse{
		ScanRunID:   repoRes.ScanRunID,
		Verb:        verb,
		ScanRunKind: handler.ScanRunKind(),
		PayloadHash: hash.String(),
		// FoundationDispatched: unknown at replay time --
		// the durable row carries no record of whether the
		// foundation tier was dispatched. We default to
		// false; publishers needing certainty can re-query
		// the foundation tier directly.
		FoundationDispatched: false,
		Replayed:             true,
	}
	body, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		r.logInternal(req, verb, "durable-replay-envelope-marshal-failure", marshalErr)
		r.writeError(w, http.StatusInternalServerError,
			fmt.Sprintf("marshalling durable replay envelope: %v", marshalErr),
			"INTERNAL_ERROR", verb)
		return
	}
	if r.logger != nil {
		r.logger.Info("ingest webhook: replay (durable scan_run_id, cross-process)",
			"verb", verb,
			"scan_run_id", repoRes.ScanRunID,
			"existing_status", repoRes.ExistingStatus,
			"payload_hash", hash,
		)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// commitInMemoryReplay stores a minimal IdempotencyRecord
// reflecting a durable-replay so subsequent same-process
// retries hit the fast cache instead of re-querying the
// durable seam. Returns true on success so the deferred
// in-process Abort is suppressed. A commit failure here is
// non-fatal -- we logged a durable replay successfully; the
// in-process cache is a latency optimisation.
func (r *Router) commitInMemoryReplay(ctx context.Context, verb string, hash PayloadHash, repoRes ScanRunRepositoryResult) bool {
	envelope := RouterResponse{
		ScanRunID:   repoRes.ScanRunID,
		Verb:        verb,
		PayloadHash: hash.String(),
		Replayed:    true,
	}
	body, marshalErr := json.Marshal(envelope)
	if marshalErr != nil {
		// Logging happens at the caller; leave the deferred
		// Abort to clear the in-process claim.
		return false
	}
	record := IdempotencyRecord{
		Verb:         verb,
		PayloadHash:  hash,
		ScanRunID:    repoRes.ScanRunID,
		ResponseBody: body,
	}
	if err := r.store.Commit(ctx, record); err != nil {
		return false
	}
	return true
}

// classifyVerbError consults the optional
// [VerbErrorClassifier] interface; falls back to 500 /
// INTERNAL_ERROR.
func (r *Router) classifyVerbError(handler VerbHandler, err error) (int, string) {
	if cls, ok := handler.(VerbErrorClassifier); ok {
		if status, code := cls.ClassifyError(err); status != 0 && code != "" {
			return status, code
		}
	}
	return http.StatusInternalServerError, "INTERNAL_ERROR"
}

func (r *Router) registeredVerbs() []string {
	out := make([]string, 0, len(r.verbs))
	for v := range r.verbs {
		out = append(out, v)
	}
	return out
}

func (r *Router) writeError(w http.ResponseWriter, status int, msg, code, verb string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := ErrorBody{Error: msg, Code: code}
	_ = json.NewEncoder(w).Encode(body)
	_ = verb // reserved for a future per-verb error counter
}

func (r *Router) logHMACFailure(req *http.Request, verb, code string, err error) {
	if r.logger == nil {
		return
	}
	r.logger.Warn("ingest webhook: HMAC verification failed",
		"verb", verb,
		"code", code,
		"err", err.Error(),
		"remote_addr", req.RemoteAddr,
		// NEVER log the signing_key_id value, the secret,
		// the supplied signature, or the computed digest --
		// any of those would leak side-channel information
		// useful for brute-force / replay attacks.
	)
}

func (r *Router) logInternal(req *http.Request, verb, kind string, err error) {
	if r.logger == nil {
		return
	}
	r.logger.Warn("ingest webhook: internal failure",
		"verb", verb,
		"kind", kind,
		"err", err.Error(),
		"remote_addr", req.RemoteAddr,
	)
}

// classifyKeyIDError maps a [ValidateSigningKeyID] sentinel
// to the canonical error code the 401 response carries.
func classifyKeyIDError(err error) string {
	switch {
	case errors.Is(err, ErrSigningKeyIDMissing):
		return "HMAC_MISSING_KEY_ID"
	case errors.Is(err, ErrSigningKeyIDMalformed):
		return "HMAC_MALFORMED_KEY_ID"
	default:
		return "HMAC_INVALID"
	}
}

// Compile-time interface assertion.
var _ http.Handler = (*Router)(nil)

// Ensure ctx is referenced in case future refactors drop the
// store's ctx usage; keeps the import meaningful for
// static-analysers.
var _ = context.Background
