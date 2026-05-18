package mgmtapi

// Stage 7.3: `mgmt.feedback` -- POST /v1/episodes/{parent_id}/feedback.
//
// architecture.md §6.2.1 / §6.2.2 pin the verb signature as
// `mgmt.feedback(parent_episode_id, outcome, corrected_action?,
// note?)` and the coupling: `outcome=human_corrected` REQUIRES
// `corrected_action`, every other outcome FORBIDS it. The
// schema mirrors that as a CHECK constraint on the `episode`
// table (`episode_corrected_action_chk`) but we reject at the
// API layer so a malformed request returns 400 with an
// actionable code instead of bubbling up a 500 from the
// constraint violation.
//
// What this verb writes (architecture.md §4.4 / §7.3)
// ---------------------------------------------------
//
//  1. One new `feedback` Episode row:
//     `kind='feedback'`, `context_id=NULL`,
//     `parent_episode_id=<parent>`, `corrected_action`=as
//     supplied (or NULL on acknowledgements).
//  2. One `EpisodeUpdate(episode_id=<parent>,
//     new_outcome=<outcome>, actor='operator', note=?)` row
//     that flips the PARENT's effective status (G3 -- the
//     parent row itself is never mutated).
//
// The synthetic positive Episode (architecture.md §4.4 step 4)
// is NOT written here -- it is the Consolidator's job on its
// next tick (Stage 6.3, see internal/consolidator/service.go).
//
// Server-side row shape (mirrors the existing integration-test
// convention in internal/consolidator/service_integration_test.go's
// `submitOperatorFeedback`):
//
//   - `episode_id`           := DB-assigned via DEFAULT
//                               gen_random_uuid(), returned via
//                               RETURNING so the response
//                               carries it.
//   - `episode_group_id`     := DB-side gen_random_uuid().
//                               Feedback is its own logical
//                               task; the synthetic positive
//                               COPIES the parent's group id,
//                               not the feedback's.
//   - `repo_id`              := COPIED from the parent so the
//                               feedback Episode shares its
//                               parent's repo scope (no
//                               cross-repo bleed).
//   - `session_id`           := "feedback:" + auth subject.
//                               Lets `mgmt.read.episodes`
//                               attribute the row to the
//                               operator who filed it.
//   - `trace_id`             := freshly-minted UUID v4. Gives
//                               this feedback call its own
//                               correlation handle in logs.
//   - `kind`                 := 'feedback'.
//   - `parent_episode_id`    := the path segment, validated as
//                               a UUID first.
//   - `context_id`           := NULL (arch §4.4 step 2).
//   - `action`               := '{"op":"feedback"}'::jsonb.
//                               The schema requires `action`
//                               NOT NULL; the consolidator only
//                               reads `corrected_action` from
//                               feedback Episodes, so this is a
//                               stable marker payload that
//                               matches the existing
//                               integration-test convention.
//   - `outcome`              := as supplied.
//   - `corrected_action`     := as supplied (when
//                               outcome=human_corrected) or
//                               NULL.
//   - `degraded`             := false (write verbs have no
//                               degraded mode per arch §6.3).
//   - `degraded_reason`      := NULL.
//
// Parent kind gate
// ----------------
// We REJECT feedback whose `parent_episode_id` points at a
// non-`agent` Episode. The architecture phrasing ("Operator
// opens an Episode in the UI and submits mgmt.feedback") leaves
// the parent kind open, but the Consolidator's Stage 6.3
// synthetic-positive promotion query
// (`internal/consolidator/service.go` -- `parent.kind =
// 'agent'`) only walks agent parents. Allowing feedback on a
// `feedback` or `synthetic_positive` parent would succeed at
// the API layer, write the two rows, and produce no synthetic
// positive on the next Consolidator tick -- a silent dead-end
// for the operator. Fail loud at the API instead.
//
// Idempotency
// -----------
// This verb is NOT idempotent: every POST appends a new
// feedback Episode and a new EpisodeUpdate. Two retries of the
// same correction produce two feedback Episodes (and the
// Consolidator emits two synthetic positives -- one per distinct
// `synthesized_from_feedback_episode_id`, per the partial
// UNIQUE on `(kind='synthetic_positive',
// synthesized_from_feedback_episode_id, created_at)`). This
// matches the e2e scenario "Multiple operators correcting the
// same parent each produce one synthetic positive" -- the
// closed-set assumption is that operators retry RARELY and the
// reranker downstream can absorb the duplicates. Future
// optimisation may add a per-(parent, operator) dedupe.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// RouteEpisodes is the prefix every /v1/episodes/* verb falls
// under. Stage 7.3 owns the single suffix `/feedback`; future
// read-side verbs (e.g. `mgmt.read.episodes` from Stage 7.5)
// land here too.
const RouteEpisodes = "/v1/episodes"

// feedbackSuffix is the trailing segment of the feedback verb's
// path. Kept lower-case to match the rest of the surface.
const feedbackSuffix = "/feedback"

// feedbackEpisodeActionJSON is the stable marker payload we
// stamp onto every feedback Episode's `action` column. The
// schema requires `action` NOT NULL; this value mirrors the
// existing consolidator integration-test convention
// (`internal/consolidator/service_integration_test.go`
// `submitOperatorFeedback`) so production and test row shapes
// agree.
const feedbackEpisodeActionJSON = `{"op":"feedback"}`

// feedbackAllowedOutcomes is the closed set the §6.2.2
// validation accepts on `mgmt.feedback`. Mirrors the `outcome`
// ENUM exactly (the operator surface accepts every outcome,
// unlike `agent.observe` which forbids `human_corrected` per
// C15).
var feedbackAllowedOutcomes = map[string]struct{}{
	"success":         {},
	"failure":         {},
	"refused":         {},
	"degraded":        {},
	"human_corrected": {},
}

// FeedbackRequest is the wire shape of POST
// /v1/episodes/{parent_id}/feedback. Field names use
// lower_snake_case to match the rest of the surface.
//
// `CorrectedAction` is decoded as json.RawMessage so the
// handler can distinguish three operator intents:
//
//   - field omitted entirely (RawMessage zero-length)
//   - field set to JSON null (`"null"`)
//   - field set to a JSON value
//
// The first two are equivalent per §6.2.2 (no
// `corrected_action` supplied). The third is required on
// `outcome=human_corrected` and forbidden otherwise.
type FeedbackRequest struct {
	// Outcome is the new effective outcome the operator
	// asserts for the parent Episode. REQUIRED. Must be one
	// of the §5.3.1 outcome ENUM members.
	Outcome string `json:"outcome"`
	// CorrectedAction is the operator-supplied replacement
	// action. REQUIRED when Outcome=human_corrected (§6.2.2);
	// MUST be absent or null otherwise. When present, MUST
	// be a JSON object -- the downstream Consolidator copies
	// this value into the synthetic_positive Episode's
	// `action` column, which is expected to match the
	// object-shaped actions that agent.observe writes.
	CorrectedAction json.RawMessage `json:"corrected_action,omitempty"`
	// Note is the operator-supplied free-text note that
	// lands on the EpisodeUpdate row. OPTIONAL. Empty is
	// stored as SQL NULL.
	Note string `json:"note,omitempty"`
}

// FeedbackResponse is the wire shape of a successful POST
// /v1/episodes/{parent_id}/feedback. The single returned id
// is the freshly-appended `feedback` Episode's
// `episode_id`. The synthetic positive Episode the
// Consolidator will produce on its next tick has its own id
// (architecture.md §4.4 step 4); operators discover it via
// `mgmt.read.episodes` once Stage 7.5 lands.
type FeedbackResponse struct {
	FeedbackEpisodeID string `json:"feedback_episode_id"`
}

// extractEpisodeFeedbackPath parses
// `/v1/episodes/{parent_id}/feedback`. Returns the raw
// parent_id string and the suffix (`/feedback`). The parent_id
// is NOT validated as a UUID here -- that's the verb handler's
// job so 400 / 404 attribution stays clean.
//
// Returns ok=false for any path that does not match the shape
// exactly (extra path segments, empty parent, missing suffix).
func extractEpisodeFeedbackPath(path string) (parentID, suffix string, ok bool) {
	rest := strings.TrimPrefix(path, RouteEpisodes+"/")
	if !strings.HasSuffix(rest, feedbackSuffix) {
		return "", "", false
	}
	id := strings.TrimSuffix(rest, feedbackSuffix)
	if id == "" || strings.Contains(id, "/") {
		return "", "", false
	}
	return id, feedbackSuffix, true
}

func (h *Handler) handleFeedback(w http.ResponseWriter, r *http.Request, rawParentID string) {
	ctx := r.Context()

	if !reUUID.MatchString(rawParentID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_parent_id",
			"parent_episode_id path segment is not a valid UUID")
		return
	}

	var req FeedbackRequest
	if !h.decodeJSONBody(w, r, &req) {
		return
	}

	if code, msg, ok := validateFeedbackRequest(&req); !ok {
		writeJSONError(w, http.StatusBadRequest, code, msg)
		return
	}

	resp, err := h.executeFeedback(ctx, rawParentID, &req)
	if err != nil {
		if errors.Is(err, errFeedbackParentNotFound) {
			writeJSONError(w, http.StatusNotFound, "episode_not_found",
				"no episode with the supplied parent_episode_id")
			return
		}
		if errors.Is(err, errFeedbackParentNotAgent) {
			writeJSONError(w, http.StatusBadRequest, "invalid_parent_kind",
				"parent_episode_id must reference an 'agent' Episode; "+
					"feedback on 'feedback' or 'synthetic_positive' parents "+
					"would not be promoted by the Consolidator")
			return
		}
		h.logger.Error("mgmtapi.feedback.db_failed",
			slog.String("op", "feedback"),
			slog.String("parent_episode_id", rawParentID),
			slog.String("outcome", req.Outcome),
			slog.String("error", err.Error()),
		)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	subject, _ := SubjectFromContext(ctx)
	h.logger.Info("mgmtapi.feedback.ok",
		slog.String("op", "feedback"),
		slog.String("parent_episode_id", rawParentID),
		slog.String("feedback_episode_id", resp.FeedbackEpisodeID),
		slog.String("outcome", req.Outcome),
		slog.Bool("had_corrected_action", correctedActionPresent(req.CorrectedAction)),
		slog.Bool("had_note", req.Note != ""),
		slog.String("subject", subject),
		slog.Time("at", h.clock()),
	)
	writeJSONResponse(w, http.StatusCreated, resp)
}

// validateFeedbackRequest applies the §6.2.2 rules. Returns
// (code, msg, true) when the request is valid; otherwise
// returns (code, msg, false) with the error code / message the
// 400 envelope should carry.
//
// Rules:
//   - outcome required, in the closed set.
//   - corrected_action coupling per §6.2.2:
//     human_corrected ⇒ corrected_action required + JSON object;
//     other outcomes ⇒ corrected_action MUST be absent or null.
func validateFeedbackRequest(req *FeedbackRequest) (code, msg string, ok bool) {
	out := strings.TrimSpace(req.Outcome)
	if out == "" {
		return "invalid_request", "outcome: required", false
	}
	if _, allowed := feedbackAllowedOutcomes[out]; !allowed {
		return "invalid_outcome",
			fmt.Sprintf("outcome %q is not one of {success, failure, refused, degraded, human_corrected}", out),
			false
	}
	req.Outcome = out

	hasCA := correctedActionPresent(req.CorrectedAction)

	switch {
	case out == "human_corrected" && !hasCA:
		return "corrected_action_required",
			"corrected_action is required when outcome=human_corrected (architecture.md §6.2.2)",
			false
	case out != "human_corrected" && hasCA:
		return "corrected_action_forbidden",
			fmt.Sprintf("corrected_action must be omitted when outcome=%q (architecture.md §6.2.2)", out),
			false
	}

	if hasCA {
		// Downstream the Consolidator copies this value into
		// `synthetic_positive.action`, which is expected to be
		// an object (the agent.observe surface always writes
		// object-shaped actions). Reject scalars / arrays /
		// strings at the API so a downstream `mgmt.read.episodes`
		// view never has to render a non-object action.
		trimmed := bytes.TrimSpace(req.CorrectedAction)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return "invalid_corrected_action",
				"corrected_action must be a JSON object",
				false
		}
		// Also guard against syntactically-broken JSON. The
		// outer body decode validated that the top-level shape
		// is parseable, but a RawMessage can carry any byte
		// sequence inside its braces -- enforce that we have a
		// real JSON object here so the `$n::jsonb` cast at
		// INSERT time does not reject with a 500.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &probe); err != nil {
			return "invalid_corrected_action",
				"corrected_action is not valid JSON",
				false
		}
	}

	return "", "", true
}

// correctedActionPresent returns true when the operator
// SUPPLIED a non-null corrected_action value. JSON omission
// (zero-length RawMessage) and explicit `null` are equivalent
// per §6.2.2 ("must be omitted").
func correctedActionPresent(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	return true
}

// errFeedbackParentNotFound is the in-package sentinel for "the
// parent_episode_id is a well-formed UUID but no Episode with
// that id exists". The handler maps it to 404 + the
// `episode_not_found` envelope code.
var errFeedbackParentNotFound = errors.New("mgmtapi: feedback: parent episode not found")

// errFeedbackParentNotAgent is the in-package sentinel for "the
// parent Episode exists but its kind is not 'agent'". The
// handler maps it to 400 + the `invalid_parent_kind` envelope
// code. See the file-header `Parent kind gate` section for
// rationale.
var errFeedbackParentNotAgent = errors.New("mgmtapi: feedback: parent episode is not of kind 'agent'")

func (h *Handler) executeFeedback(ctx context.Context, parentID string, req *FeedbackRequest) (FeedbackResponse, error) {
	tx, err := h.db.BeginTx(ctx, nil)
	if err != nil {
		return FeedbackResponse{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // commit makes rollback a no-op

	// Step 1: load the parent's repo_id + kind. The feedback
	// Episode's repo_id MUST match the parent's so the
	// `mgmt.read.episodes(repo_id=...)` filter returns the
	// feedback alongside the parent. Loading `kind` lets us
	// enforce the agent-only parent gate (see header).
	//
	// Note on performance: the episode table is partitioned
	// monthly on created_at. A `WHERE episode_id = $1` lookup
	// without a `created_at` predicate scans every partition
	// (the composite PK is `(episode_id, created_at)`, so each
	// partition's PK index seeks but partition pruning does
	// not engage). This is acceptable for an operator-driven
	// verb whose call rate is human-scale.
	const loadParent = `
		SELECT repo_id::text, kind::text
		FROM episode
		WHERE episode_id = $1::uuid
		LIMIT 1
	`
	var parentRepoID, parentKind string
	err = tx.QueryRowContext(ctx, loadParent, parentID).Scan(&parentRepoID, &parentKind)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return FeedbackResponse{}, errFeedbackParentNotFound
	case err != nil:
		if isInvalidUUIDError(err) {
			return FeedbackResponse{}, errFeedbackParentNotFound
		}
		return FeedbackResponse{}, fmt.Errorf("load parent episode: %w", err)
	}

	if parentKind != "agent" {
		return FeedbackResponse{}, errFeedbackParentNotAgent
	}

	// Step 2: insert the feedback Episode. We let the DB
	// default `episode_id` via gen_random_uuid() and capture
	// the assigned value via RETURNING -- mgmt.feedback has no
	// WAL fallback so there's no need to mint Go-side (unlike
	// agent.observe which pre-mints for §7.5 replay
	// determinism).
	//
	// We pass corrected_action as a string (not as a
	// json.RawMessage / []byte) because lib/pq binds []byte
	// as bytea, which the `$n::jsonb` cast then rejects.
	// `nil` translates to SQL NULL so the
	// `episode_corrected_action_chk` (human_corrected IFF
	// corrected_action NOT NULL) is satisfied for both
	// branches.
	traceID, err := newFeedbackTraceID()
	if err != nil {
		return FeedbackResponse{}, fmt.Errorf("mint trace_id: %w", err)
	}
	subject, _ := SubjectFromContext(ctx)
	if subject == "" {
		subject = "unknown"
	}
	sessionID := "feedback:" + subject

	var correctedActionArg any
	if correctedActionPresent(req.CorrectedAction) {
		correctedActionArg = string(bytes.TrimSpace(req.CorrectedAction))
	}

	const insertFeedback = `
		INSERT INTO episode (
		    episode_group_id, repo_id, session_id, trace_id, kind,
		    parent_episode_id, action, outcome, corrected_action
		)
		VALUES (
		    gen_random_uuid(), $1::uuid, $2, $3, 'feedback'::episode_kind,
		    $4::uuid, $5::jsonb, $6::outcome, $7::jsonb
		)
		RETURNING episode_id::text
	`
	var feedbackEpisodeID string
	if err := tx.QueryRowContext(ctx, insertFeedback,
		parentRepoID, sessionID, traceID,
		parentID, feedbackEpisodeActionJSON, req.Outcome, correctedActionArg,
	).Scan(&feedbackEpisodeID); err != nil {
		return FeedbackResponse{}, fmt.Errorf("insert feedback episode: %w", err)
	}

	// Step 3: append the EpisodeUpdate. actor='operator'
	// per architecture §5.3.2 (the actor enum's
	// human-driven member). Empty notes land as SQL NULL via
	// the type-aware sql.NullString.
	var noteArg sql.NullString
	if req.Note != "" {
		noteArg = sql.NullString{String: req.Note, Valid: true}
	}
	const insertUpdate = `
		INSERT INTO episode_update (episode_id, new_outcome, note, actor)
		VALUES ($1::uuid, $2::outcome, $3, 'operator'::actor)
	`
	if _, err := tx.ExecContext(ctx, insertUpdate,
		parentID, req.Outcome, noteArg,
	); err != nil {
		return FeedbackResponse{}, fmt.Errorf("insert episode_update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return FeedbackResponse{}, fmt.Errorf("commit tx: %w", err)
	}
	return FeedbackResponse{FeedbackEpisodeID: feedbackEpisodeID}, nil
}

// newFeedbackTraceID mints a fresh RFC 4122 v4 UUID for use as
// the feedback Episode's trace_id. Kept package-internal (vs.
// pulling in google/uuid) to keep the dependency surface
// tight; identical in shape to agentapi.newUUIDv4 but
// duplicated here so the mgmtapi package has no cross-package
// internal dependency.
func newFeedbackTraceID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("read random: %w", err)
	}
	// RFC 4122 §4.4: set the version (high nibble of octet 6
	// to 0100b) and the variant (high two bits of octet 8 to
	// 10b).
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var buf [36]byte
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf[:]), nil
}
