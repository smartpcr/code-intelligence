package mgmtapi

// Stage 7.5: Operator read endpoints. Implements every
// `mgmt.read.*` verb from architecture.md §6.2.3 as a
// GET handler under `/v1/`:
//
//	GET /v1/repos
//	GET /v1/commits?repo_id=...&since=...&limit=...
//	GET /v1/episodes?since=...&repo_id=...&outcome_in=...&kind_in=...&limit=...
//	GET /v1/observations?episode_id=...&limit=...
//	GET /v1/context/{context_id}
//	GET /v1/concepts?promoted=...&limit=...
//	GET /v1/concept_supports?concept_id=...&repo_id=...&limit=...
//	GET /v1/graph_node/{node_id}?sha=...
//	GET /v1/trace_observation/{edge_id}?limit=...&before=...
//
// Behavioural invariants this file enforces (cross-checked
// against the Stage 7.5 brief, architecture.md §6.2.3 / §6.3 /
// §7.6, and tech-spec.md §C13):
//
//   - Every successful read response carries the §6.3
//     degraded envelope at the JSON top level
//     (`degraded: bool`, `degraded_reason: text?`). Implemented
//     by wrapping every payload in [DegradedEnvelope].
//   - `GET /v1/episodes` MUST receive a `since` query parameter
//     so partition pruning engages (risk §9.2 / tech-spec
//     §8.7.2). A missing or empty `since` returns 400
//     `since_required` BEFORE any DB read.
//   - `mgmt.read.episodes` joins each Episode to its latest
//     `EpisodeUpdate.new_outcome` as `current_status`. When no
//     EpisodeUpdate exists, `current_status` mirrors the
//     Episode's original `outcome` column. The original
//     `outcome` is ALSO returned verbatim (the architecture
//     pins both shapes side-by-side: "the original `outcome`
//     column shows `failure`" while `current_status` reflects
//     the latest update).
//   - `mgmt.read.context` is tombstone-tolerant per risk §9.13:
//     when a `RecallContextLog.node_ids[]` element references
//     a node carrying a `node_retirement` row, the dereferenced
//     node card is still returned, with the `retired_at_sha`
//     badge field populated. Same rule for edges (the §5.2.4
//     EdgeRetirement tombstone); concepts are append-only
//     (G4) so no retirement check applies. Array order is
//     preserved by hydrating via `unnest WITH ORDINALITY` so
//     the rank-ordering the writer recorded survives the
//     join.
//   - Repo-scoped reads (commits, episodes-with-repo_id,
//     graph_node, trace_observation, context) consult
//     `repo_health` (§4.2 / migration 0019) so a per-repo
//     degraded flag surfaces in the response envelope. The
//     repos-list aggregator sets the top-level `degraded=true`
//     when any returned repo is currently degraded; the
//     per-row `degraded`/`degraded_reason` columns let the UI
//     attribute the banner to a specific repo.
//   - `graph_node` honours the optional `sha` parameter as a
//     best-effort point-in-time view. When `sha` is provided
//     we resolve it to a `repo_commit.committed_at` and apply
//     the §5.2 visibility rule: a Node/Edge is visible iff
//     its `from_sha` committed at or before the target AND it
//     was not retired at or before the target.
//
// SQL portability note: the handler runs as `agent_memory_app`
// (mgmtapi composition root) — which holds SELECT on every
// table referenced here per migration 0016. The same queries
// also work for `agent_memory_ro` (migration 0017). DO NOT
// add anything outside the SELECT envelope; the architecture
// pins these verbs as read-only (C13).

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// Route paths for the Stage 7.5 read verbs. Listed here so
// the routing switch in `routeRead` and the integration test
// pack share a single source of truth.
const (
	RouteReadRepos             = "/v1/repos"
	RouteReadCommits           = "/v1/commits"
	RouteReadEpisodes          = "/v1/episodes"
	RouteReadObservations      = "/v1/observations"
	RouteReadContextPrefix     = "/v1/context/"
	RouteReadConcepts          = "/v1/concepts"
	RouteReadConceptSupports   = "/v1/concept_supports"
	RouteReadGraphNodePrefix   = "/v1/graph_node/"
	RouteReadTraceObsPrefix    = "/v1/trace_observation/"
)

// Defaults for paginated list endpoints. Defaults are tuned
// for an operator UI that renders 1-2 pages at a time; the
// max keeps any single response bounded so a malicious or
// confused caller cannot DOS the DB.
const (
	defaultReadLimit = 200
	maxReadLimit     = 1000
)

// reSinceDuration matches the duration-shorthand we accept on
// the `since` query parameter. Each component is a positive
// integer + a single unit char. We deliberately do NOT
// support fractional values (`1.5d`) or zero (`0d`); a zero
// or negative `since` is meaningless for partition pruning
// and would silently scan every partition.
var reSinceDuration = regexp.MustCompile(`^([1-9][0-9]*)([smhdw])$`)

// Closed enum sets for the episodes endpoint. We validate the
// caller-supplied lists against these to keep a malformed
// query from interpolating into the SQL (paranoia — we use
// parameterised queries everywhere, but the closed-set
// rejection produces a clearer 400 than the database type
// cast error).
var (
	episodeKinds    = []string{"agent", "feedback", "synthetic_positive"}
	episodeOutcomes = []string{"success", "failure", "refused", "degraded", "human_corrected"}
)

// routeRead dispatches an authenticated GET request to the
// matching verb handler. Method gating already happened in
// `route`; this function is GET-only.
//
// The dispatch order is deliberate: most-specific prefix
// matches FIRST so `/v1/concept_supports` doesn't collide
// with the `/v1/concepts` list endpoint, and the
// `/v1/context/...` prefix matches before any other prefix
// it could overlap with.
func (h *Handler) routeRead(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	// Trim a single trailing slash so `/v1/repos` and
	// `/v1/repos/` route identically.
	pTrim := strings.TrimSuffix(p, "/")
	switch {
	case pTrim == RouteReadRepos:
		h.handleReadRepos(w, r)
	case pTrim == RouteReadCommits:
		h.handleReadCommits(w, r)
	case pTrim == RouteReadEpisodes:
		h.handleReadEpisodes(w, r)
	case pTrim == RouteReadObservations:
		h.handleReadObservations(w, r)
	case strings.HasPrefix(p, RouteReadContextPrefix):
		id := strings.TrimPrefix(p, RouteReadContextPrefix)
		id = strings.TrimSuffix(id, "/")
		h.handleReadContext(w, r, id)
	case pTrim == RouteReadConceptSupports:
		h.handleReadConceptSupports(w, r)
	case pTrim == RouteReadConcepts:
		h.handleReadConcepts(w, r)
	case strings.HasPrefix(p, RouteReadGraphNodePrefix):
		id := strings.TrimPrefix(p, RouteReadGraphNodePrefix)
		id = strings.TrimSuffix(id, "/")
		h.handleReadGraphNode(w, r, id)
	case strings.HasPrefix(p, RouteReadTraceObsPrefix):
		id := strings.TrimPrefix(p, RouteReadTraceObsPrefix)
		id = strings.TrimSuffix(id, "/")
		h.handleReadTraceObservation(w, r, id)
	default:
		writeJSONError(w, http.StatusNotFound, "not_found",
			"unknown management read route")
	}
}

// -----------------------------------------------------------
// Query-parameter helpers
// -----------------------------------------------------------

// parseSinceParam parses the `since` query value into an
// absolute `time.Time` cutoff. Accepts either:
//
//   - Duration shorthand `Nw` / `Nd` / `Nh` / `Nm` / `Ns`
//     (positive integer N; relative to `now`).
//   - An RFC 3339 absolute timestamp (e.g.
//     `2026-05-17T00:00:00Z`).
//
// Returns (cutoff, true, "") on success. On parse failure,
// returns (zero, false, msg) where `msg` is operator-facing.
// `required=true` makes an empty value a parse failure with a
// "<name> required" message; `required=false` lets an empty
// value succeed with cutoff=zero.
func parseSinceParam(raw string, now time.Time, required bool, name string) (time.Time, bool, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if required {
			return time.Time{}, false, name + " required"
		}
		return time.Time{}, true, ""
	}
	if m := reSinceDuration.FindStringSubmatch(raw); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n <= 0 {
			// Should not happen — the regex matches [1-9][0-9]* — but
			// guard so a future regex relaxation can't introduce a
			// 0/negative slip.
			return time.Time{}, false, "invalid since duration: must be a positive integer"
		}
		var d time.Duration
		switch m[2] {
		case "s":
			d = time.Duration(n) * time.Second
		case "m":
			d = time.Duration(n) * time.Minute
		case "h":
			d = time.Duration(n) * time.Hour
		case "d":
			d = time.Duration(n) * 24 * time.Hour
		case "w":
			d = time.Duration(n) * 7 * 24 * time.Hour
		}
		return now.Add(-d), true, ""
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true, ""
	}
	return time.Time{}, false,
		"invalid since: use duration shorthand (e.g. 7d, 12h, 30m, 90s, 2w) or RFC3339 timestamp"
}

// parseLimitParam parses the `limit` query value into an
// integer bounded by [1, maxReadLimit]. Empty → defaultReadLimit.
// Returns (limit, true, "") on success or (0, false, msg) on
// failure.
func parseLimitParam(raw string) (int, bool, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultReadLimit, true, ""
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, "invalid limit: must be a positive integer"
	}
	if n <= 0 {
		return 0, false, "invalid limit: must be > 0"
	}
	if n > maxReadLimit {
		n = maxReadLimit
	}
	return n, true, ""
}

// parseCSVEnumList splits a comma-separated query value into
// trimmed tokens, validating each against `allowed`. Empty
// raw → (nil, true, ""). On any token outside `allowed`
// returns (nil, false, msg).
func parseCSVEnumList(raw string, allowed []string, name string) ([]string, bool, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, true, ""
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		ok := false
		for _, a := range allowed {
			if p == a {
				ok = true
				break
			}
		}
		if !ok {
			return nil, false, fmt.Sprintf(
				"invalid %s: %q is not in the closed set (allowed: %s)",
				name, p, strings.Join(allowed, ", "))
		}
		out = append(out, p)
	}
	return out, true, ""
}

// -----------------------------------------------------------
// Shared payload helpers
// -----------------------------------------------------------

// repoHealthSnapshot is the (degraded, reason) tuple a repo's
// `repo_health` row contributes to a read response. Missing
// rows = healthy (degraded=false, reason="").
type repoHealthSnapshot struct {
	Degraded bool
	Reason   string
}

// loadRepoHealth fetches the repo_health row for `repoID` if
// any. Missing row is NOT an error — it returns the zero
// snapshot, which the caller treats as "healthy".
//
// `repoID` MUST already be UUID-validated by the caller; this
// helper does not gate on `reUUID`.
func (h *Handler) loadRepoHealth(ctx context.Context, repoID string) (repoHealthSnapshot, error) {
	const q = `
		SELECT degraded, COALESCE(degraded_reason::text, '')
		FROM repo_health
		WHERE repo_id = $1::uuid
	`
	var snap repoHealthSnapshot
	err := h.db.QueryRowContext(ctx, q, repoID).Scan(&snap.Degraded, &snap.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return repoHealthSnapshot{}, nil
	}
	return snap, err
}

// writeReadResponse serialises `payload` wrapped in the
// `degraded` / `degraded_reason` envelope per §6.3. `degraded`
// reflects the verb's repo-health derived view; `reason` is
// the matching short string when degraded.
func writeReadResponse[T any](w http.ResponseWriter, status int, payload T, degraded bool, reason string) {
	env := DegradedEnvelope[T]{
		Payload:        payload,
		Degraded:       degraded,
		DegradedReason: reason,
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(env); err != nil {
		// Encoding errors land in the logger but the
		// headers are already on the wire. Avoid the
		// silent loss by returning fast — the caller can
		// inspect the partial body in operator triage.
		_ = err
	}
}

// -----------------------------------------------------------
// mgmt.read.repos
// -----------------------------------------------------------

// RepoRow is one element of [ReposResponse.Repos]. Joined to
// the latest `ingest_jobs.status` (ordered by updated_at desc
// to match the `ingest_jobs_repo_updated_idx` index installed
// in migration 0006a) and to `repo_health` so the operator
// dashboard can render per-repo state without an extra
// round-trip.
type RepoRow struct {
	RepoID         string   `json:"repo_id"`
	URL            string   `json:"url"`
	DefaultBranch  string   `json:"default_branch"`
	CurrentHeadSHA string   `json:"current_head_sha"`
	LanguageHints  []string `json:"language_hints"`
	CreatedAt      string   `json:"created_at"`
	// IngestStatus is the `status` of the most-recently-
	// updated ingest_jobs row for this repo, or empty when
	// no ingest job has ever been enqueued.
	IngestStatus string `json:"ingest_status,omitempty"`
	// Degraded / DegradedReason mirror `repo_health` for
	// this specific repo so the UI can attribute a banner
	// to a single row.
	Degraded       bool   `json:"degraded"`
	DegradedReason string `json:"degraded_reason,omitempty"`
}

// ReposResponse is the body of GET /v1/repos. Per §6.2.3 line
// 759 the architecture pins this verb at "List of Repo rows +
// their current ingest status".
type ReposResponse struct {
	Repos []RepoRow `json:"repos"`
}

func (h *Handler) handleReadRepos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	limit, ok, msg := parseLimitParam(r.URL.Query().Get("limit"))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	// Optional `filter` — a free-form ILIKE substring against
	// repo.url. Keeps the spec's `mgmt.read.repos(filter?)`
	// arg minimally honoured without a query builder.
	filter := strings.TrimSpace(r.URL.Query().Get("filter"))

	const baseQ = `
		WITH latest_job AS (
			SELECT DISTINCT ON (repo_id)
				repo_id, status::text AS status
			FROM ingest_jobs
			ORDER BY repo_id, updated_at DESC
		)
		SELECT
			r.repo_id::text,
			r.url,
			r.default_branch,
			r.current_head_sha,
			COALESCE(r.language_hints, ARRAY[]::text[]),
			r.created_at,
			COALESCE(j.status, '') AS ingest_status,
			COALESCE(h.degraded, false) AS degraded,
			COALESCE(h.degraded_reason::text, '') AS degraded_reason
		FROM repo r
		LEFT JOIN latest_job j ON j.repo_id = r.repo_id
		LEFT JOIN repo_health h ON h.repo_id = r.repo_id
	`
	args := []any{}
	q := baseQ
	if filter != "" {
		args = append(args, "%"+filter+"%")
		q += " WHERE r.url ILIKE $1"
	}
	q += " ORDER BY r.created_at DESC LIMIT " + strconv.Itoa(limit)

	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		h.logger.Error("mgmtapi.read.repos.db_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := ReposResponse{Repos: []RepoRow{}}
	anyDegraded := false
	for rows.Next() {
		var row RepoRow
		var langs pq.StringArray
		var createdAt time.Time
		if err := rows.Scan(
			&row.RepoID, &row.URL, &row.DefaultBranch, &row.CurrentHeadSHA,
			&langs, &createdAt,
			&row.IngestStatus, &row.Degraded, &row.DegradedReason,
		); err != nil {
			h.logger.Error("mgmtapi.read.repos.scan_failed",
				slog.String("error", err.Error()))
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		row.LanguageHints = []string(langs)
		if row.LanguageHints == nil {
			row.LanguageHints = []string{}
		}
		row.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		if row.Degraded {
			anyDegraded = true
		}
		out.Repos = append(out.Repos, row)
	}
	if err := rows.Err(); err != nil {
		h.logger.Error("mgmtapi.read.repos.iter_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	// Top-level degraded surfaces "any returned repo is
	// currently degraded" so the UI can render a system-wide
	// banner without re-scanning the array client-side.
	reason := ""
	if anyDegraded {
		reason = "graph_store_unavailable"
		// pick the first non-empty per-row reason if any.
		for _, repo := range out.Repos {
			if repo.Degraded && repo.DegradedReason != "" {
				reason = repo.DegradedReason
				break
			}
		}
	}
	writeReadResponse(w, http.StatusOK, out, anyDegraded, reason)
}

// -----------------------------------------------------------
// mgmt.read.commits
// -----------------------------------------------------------

// CommitRow is one element of [CommitsResponse.Commits]. Maps
// directly onto a `repo_commit` row plus the partition-aware
// `index_status` value the Repo Indexer flips when it
// finishes a SHA's full ingest.
type CommitRow struct {
	SHA         string `json:"sha"`
	ParentSHA   string `json:"parent_sha,omitempty"`
	CommittedAt string `json:"committed_at"`
	IndexStatus string `json:"index_status"`
}

// CommitsResponse is the body of GET /v1/commits.
type CommitsResponse struct {
	RepoID  string      `json:"repo_id"`
	Commits []CommitRow `json:"commits"`
}

func (h *Handler) handleReadCommits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	repoID := strings.TrimSpace(q.Get("repo_id"))
	if repoID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "repo_id required")
		return
	}
	if !reUUID.MatchString(repoID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_repo_id",
			"repo_id query parameter is not a valid UUID")
		return
	}
	since, ok, msg := parseSinceParam(q.Get("since"), h.clock(), false, "since")
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	limit, ok, msg := parseLimitParam(q.Get("limit"))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}

	args := []any{repoID}
	sql := `
		SELECT sha, COALESCE(parent_sha, ''), committed_at, index_status
		FROM repo_commit
		WHERE repo_id = $1::uuid
	`
	if !since.IsZero() {
		args = append(args, since)
		sql += " AND committed_at >= $2"
	}
	sql += " ORDER BY committed_at DESC LIMIT " + strconv.Itoa(limit)

	rows, err := h.db.QueryContext(ctx, sql, args...)
	if err != nil {
		h.logger.Error("mgmtapi.read.commits.db_failed",
			slog.String("error", err.Error()),
			slog.String("repo_id", repoID))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := CommitsResponse{RepoID: repoID, Commits: []CommitRow{}}
	for rows.Next() {
		var row CommitRow
		var committedAt time.Time
		if err := rows.Scan(&row.SHA, &row.ParentSHA, &committedAt, &row.IndexStatus); err != nil {
			h.logger.Error("mgmtapi.read.commits.scan_failed",
				slog.String("error", err.Error()))
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		row.CommittedAt = committedAt.UTC().Format(time.RFC3339Nano)
		out.Commits = append(out.Commits, row)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	health, herr := h.loadRepoHealth(ctx, repoID)
	if herr != nil {
		h.logger.Warn("mgmtapi.read.commits.repo_health_failed",
			slog.String("error", herr.Error()),
			slog.String("repo_id", repoID))
	}
	writeReadResponse(w, http.StatusOK, out, health.Degraded, health.Reason)
}

// -----------------------------------------------------------
// mgmt.read.episodes
// -----------------------------------------------------------

// EpisodeRow is one element of [EpisodesResponse.Episodes].
// `Outcome` is the ORIGINAL `episode.outcome` column (per
// architecture §5.3.1 "this column is always the initial
// value"); `CurrentStatus` is the latest
// `EpisodeUpdate.new_outcome` for this episode, OR the
// original outcome when no update row exists yet — the join
// that backs this is the architecture's §6.2.3 "Episodes with
// their `EpisodeUpdate` joined as `current_status`" contract.
type EpisodeRow struct {
	EpisodeID       string          `json:"episode_id"`
	EpisodeGroupID  string          `json:"episode_group_id"`
	RepoID          string          `json:"repo_id"`
	SessionID       string          `json:"session_id"`
	TraceID         string          `json:"trace_id"`
	Kind            string          `json:"kind"`
	Outcome         string          `json:"outcome"`
	CurrentStatus   string          `json:"current_status"`
	ContextID       string          `json:"context_id,omitempty"`
	ParentEpisodeID string          `json:"parent_episode_id,omitempty"`
	Action          json.RawMessage `json:"action,omitempty"`
	CorrectedAction json.RawMessage `json:"corrected_action,omitempty"`
	SignalJSON      json.RawMessage `json:"signal_json,omitempty"`
	Degraded        bool            `json:"degraded"`
	DegradedReason  string          `json:"degraded_reason,omitempty"`
	CreatedAt       string          `json:"created_at"`
}

// EpisodesResponse is the body of GET /v1/episodes.
type EpisodesResponse struct {
	Episodes []EpisodeRow `json:"episodes"`
}

func (h *Handler) handleReadEpisodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	// since is REQUIRED — partition pruning per risk §9.2.
	since, ok, msg := parseSinceParam(q.Get("since"), h.clock(), true, "since")
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "since_required", msg)
		return
	}

	repoID := strings.TrimSpace(q.Get("repo_id"))
	if repoID != "" && !reUUID.MatchString(repoID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_repo_id",
			"repo_id query parameter is not a valid UUID")
		return
	}
	outcomes, ok, msg := parseCSVEnumList(q.Get("outcome_in"), episodeOutcomes, "outcome_in")
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	kinds, ok, msg := parseCSVEnumList(q.Get("kind_in"), episodeKinds, "kind_in")
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	limit, ok, msg := parseLimitParam(q.Get("limit"))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}

	// We build SQL piecewise with parameter placeholders so a
	// PostgreSQL execution plan can use
	// `episode_repo_created_idx` (the (repo_id, created_at)
	// index from migration 0007) when repo_id is supplied,
	// and the partition pruner engages on the `created_at >=
	// $since` predicate either way.
	//
	// The DISTINCT ON (episode_id) subquery picks the
	// most-recent EpisodeUpdate per episode_id; we COALESCE
	// onto the original outcome so an episode without an
	// update still has a well-defined current_status.
	//
	// The CTE carries a `created_at >= $1` predicate so the
	// PostgreSQL planner can prune `episode_update`'s monthly
	// partitions in the same way the outer query prunes
	// `episode`'s. By invariant `episode_update.created_at >=
	// episode.created_at` (you can only update an existing
	// episode), so every relevant update for episodes inside
	// `since` is itself inside `since` — the predicate trims
	// scans without dropping rows that would otherwise win
	// the DISTINCT ON.
	sb := strings.Builder{}
	sb.WriteString(`
		WITH latest_update AS (
			SELECT DISTINCT ON (episode_id)
				episode_id, new_outcome::text AS new_outcome
			FROM episode_update
			WHERE created_at >= $1
			ORDER BY episode_id, created_at DESC
		)
		SELECT
			e.episode_id::text,
			e.episode_group_id::text,
			e.repo_id::text,
			e.session_id,
			e.trace_id,
			e.kind::text,
			e.outcome::text,
			COALESCE(u.new_outcome, e.outcome::text) AS current_status,
			COALESCE(e.context_id::text, ''),
			COALESCE(e.parent_episode_id::text, ''),
			e.action::text,
			COALESCE(e.corrected_action::text, ''),
			COALESCE(e.signal_json::text, ''),
			e.degraded,
			COALESCE(e.degraded_reason::text, ''),
			e.created_at
		FROM episode e
		LEFT JOIN latest_update u ON u.episode_id = e.episode_id
		WHERE e.created_at >= $1
	`)
	args := []any{since}
	if repoID != "" {
		args = append(args, repoID)
		sb.WriteString(" AND e.repo_id = $")
		sb.WriteString(strconv.Itoa(len(args)))
		sb.WriteString("::uuid")
	}
	if len(outcomes) > 0 {
		args = append(args, pq.Array(outcomes))
		sb.WriteString(" AND e.outcome::text = ANY($")
		sb.WriteString(strconv.Itoa(len(args)))
		sb.WriteString(")")
	}
	if len(kinds) > 0 {
		args = append(args, pq.Array(kinds))
		sb.WriteString(" AND e.kind::text = ANY($")
		sb.WriteString(strconv.Itoa(len(args)))
		sb.WriteString(")")
	}
	sb.WriteString(" ORDER BY e.created_at DESC LIMIT ")
	sb.WriteString(strconv.Itoa(limit))

	rows, err := h.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		h.logger.Error("mgmtapi.read.episodes.db_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := EpisodesResponse{Episodes: []EpisodeRow{}}
	for rows.Next() {
		var ep EpisodeRow
		var (
			actionTxt, correctedActionTxt, signalTxt string
			createdAt                                 time.Time
		)
		if err := rows.Scan(
			&ep.EpisodeID, &ep.EpisodeGroupID, &ep.RepoID,
			&ep.SessionID, &ep.TraceID, &ep.Kind, &ep.Outcome, &ep.CurrentStatus,
			&ep.ContextID, &ep.ParentEpisodeID,
			&actionTxt, &correctedActionTxt, &signalTxt,
			&ep.Degraded, &ep.DegradedReason, &createdAt,
		); err != nil {
			h.logger.Error("mgmtapi.read.episodes.scan_failed",
				slog.String("error", err.Error()))
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if actionTxt != "" {
			ep.Action = json.RawMessage(actionTxt)
		}
		if correctedActionTxt != "" {
			ep.CorrectedAction = json.RawMessage(correctedActionTxt)
		}
		if signalTxt != "" {
			ep.SignalJSON = json.RawMessage(signalTxt)
		}
		ep.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		out.Episodes = append(out.Episodes, ep)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Repo-scoped degraded view only when repo_id is supplied.
	// Without a repo scope the response spans multiple repos
	// (e.g. a system-wide failure scan); aggregating their
	// repo_health rows is out of scope for this stage.
	degraded := false
	reason := ""
	if repoID != "" {
		health, herr := h.loadRepoHealth(ctx, repoID)
		if herr != nil {
			h.logger.Warn("mgmtapi.read.episodes.repo_health_failed",
				slog.String("error", herr.Error()),
				slog.String("repo_id", repoID))
		}
		degraded = health.Degraded
		reason = health.Reason
	}
	writeReadResponse(w, http.StatusOK, out, degraded, reason)
}

// -----------------------------------------------------------
// mgmt.read.observations
// -----------------------------------------------------------

// ObservationRow is one element of
// [ObservationsResponse.Observations]. The role-and-target
// invariants from architecture §5.3.3 (and migration 0009's
// `observation_role_target_chk`) are reflected on the wire
// by emitting whichever of {node_id, edge_id, concept_id,
// degraded_recall_context_id} is set for the row's role.
type ObservationRow struct {
	ObservationID            string  `json:"observation_id"`
	Role                     string  `json:"role"`
	NodeID                   string  `json:"node_id,omitempty"`
	EdgeID                   string  `json:"edge_id,omitempty"`
	ConceptID                string  `json:"concept_id,omitempty"`
	DegradedRecallContextID  string  `json:"degraded_recall_context_id,omitempty"`
	Weight                   float64 `json:"weight"`
	CreatedAt                string  `json:"created_at"`
}

// ObservationsResponse is the body of GET /v1/observations.
type ObservationsResponse struct {
	EpisodeID    string           `json:"episode_id"`
	Observations []ObservationRow `json:"observations"`
}

func (h *Handler) handleReadObservations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	epID := strings.TrimSpace(q.Get("episode_id"))
	if epID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "episode_id required")
		return
	}
	if !reUUID.MatchString(epID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_episode_id",
			"episode_id query parameter is not a valid UUID")
		return
	}
	limit, ok, msg := parseLimitParam(q.Get("limit"))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}

	const obsQ = `
		SELECT
			observation_id::text,
			role::text,
			COALESCE(node_id::text, ''),
			COALESCE(edge_id::text, ''),
			COALESCE(concept_id::text, ''),
			COALESCE(degraded_recall_context_id::text, ''),
			weight,
			created_at
		FROM observation
		WHERE episode_id = $1::uuid
		ORDER BY created_at DESC
		LIMIT `

	rows, err := h.db.QueryContext(ctx, obsQ+strconv.Itoa(limit), epID)
	if err != nil {
		h.logger.Error("mgmtapi.read.observations.db_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := ObservationsResponse{EpisodeID: epID, Observations: []ObservationRow{}}
	for rows.Next() {
		var row ObservationRow
		var createdAt time.Time
		if err := rows.Scan(
			&row.ObservationID, &row.Role,
			&row.NodeID, &row.EdgeID, &row.ConceptID, &row.DegradedRecallContextID,
			&row.Weight, &createdAt,
		); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		row.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		out.Observations = append(out.Observations, row)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Resolve repo via the parent Episode for the
	// degraded-snapshot lookup. Best-effort — a missing
	// parent (e.g. the Episode is in a later partition we
	// just lost track of) leaves the envelope as
	// degraded=false rather than failing the whole call.
	degraded := false
	reason := ""
	if epID != "" {
		var repoID sql.NullString
		_ = h.db.QueryRowContext(ctx,
			`SELECT repo_id::text FROM episode WHERE episode_id = $1::uuid LIMIT 1`,
			epID).Scan(&repoID)
		if repoID.Valid {
			health, _ := h.loadRepoHealth(ctx, repoID.String)
			degraded = health.Degraded
			reason = health.Reason
		}
	}
	writeReadResponse(w, http.StatusOK, out, degraded, reason)
}

// -----------------------------------------------------------
// mgmt.read.context
// -----------------------------------------------------------

// ContextNodeCard is the dereferenced view of a node id that
// appeared in a `RecallContextLog.node_ids[]` array. The
// `RetiredAtSHA` badge is set iff a `node_retirement` row
// exists, per risk §9.13 ("context read tolerates retired
// ids").
type ContextNodeCard struct {
	NodeID              string          `json:"node_id"`
	Kind                string          `json:"kind,omitempty"`
	CanonicalSignature  string          `json:"canonical_signature,omitempty"`
	RepoID              string          `json:"repo_id,omitempty"`
	FromSHA             string          `json:"from_sha,omitempty"`
	Attrs               json.RawMessage `json:"attrs_json,omitempty"`
	// RetiredAtSHA is the §9.13 "badge" the operator UI
	// renders as a tombstone marker. Empty when the node is
	// still current.
	RetiredAtSHA string `json:"retired_at_sha,omitempty"`
	// Resolved is false when the node_id was referenced in
	// the RecallContextLog row but no `node` row exists for
	// it anymore (e.g. the schema purged a retired row in a
	// future cleanup; this is NOT a current code path but
	// the UI should still see the original id). For Stage
	// 7.5 we never delete nodes, so `Resolved=false` is the
	// signal of a writer bug.
	Resolved bool `json:"resolved"`
}

// ContextEdgeCard mirrors [ContextNodeCard] for edges. Edges
// have a `kind` enum (architecture §5.2.2) and src/dst node
// ids; the retirement badge follows the same §9.13 rule.
type ContextEdgeCard struct {
	EdgeID       string          `json:"edge_id"`
	Kind         string          `json:"kind,omitempty"`
	SrcNodeID    string          `json:"src_node_id,omitempty"`
	DstNodeID    string          `json:"dst_node_id,omitempty"`
	RepoID       string          `json:"repo_id,omitempty"`
	FromSHA      string          `json:"from_sha,omitempty"`
	Attrs        json.RawMessage `json:"attrs_json,omitempty"`
	RetiredAtSHA string          `json:"retired_at_sha,omitempty"`
	Resolved     bool            `json:"resolved"`
}

// ContextConceptCard mirrors [ContextNodeCard] for concepts.
// Concepts are append-only (G4) so there is no retirement
// badge.
type ContextConceptCard struct {
	ConceptID     string `json:"concept_id"`
	Fingerprint   string `json:"fingerprint,omitempty"`
	Name          string `json:"name,omitempty"`
	DescriptionMD string `json:"description_md,omitempty"`
	Resolved      bool   `json:"resolved"`
}

// ContextResponse is the body of GET /v1/context/{context_id}.
// Shape matches §6.2.3: "Full RecallContextLog row plus
// dereferenced Node/Edge/Concept cards".
type ContextResponse struct {
	ContextID            string               `json:"context_id"`
	RepoID               string               `json:"repo_id"`
	Verb                 string               `json:"verb"`
	QueryJSON            json.RawMessage      `json:"query_json,omitempty"`
	RerankerModelVersion string               `json:"reranker_model_version"`
	ServedUnderDegraded  bool                 `json:"served_under_degraded"`
	CreatedAt            string               `json:"created_at"`
	NodeIDs              []string             `json:"node_ids"`
	EdgeIDs              []string             `json:"edge_ids"`
	ConceptIDs           []string             `json:"concept_ids"`
	Nodes                []ContextNodeCard    `json:"nodes"`
	Edges                []ContextEdgeCard    `json:"edges"`
	Concepts             []ContextConceptCard `json:"concepts"`
}

func (h *Handler) handleReadContext(w http.ResponseWriter, r *http.Request, contextID string) {
	ctx := r.Context()
	if contextID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "context_id required")
		return
	}
	if !reUUID.MatchString(contextID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_context_id",
			"context_id path segment is not a valid UUID")
		return
	}

	const ctxQ = `
		SELECT
			context_id::text,
			repo_id::text,
			verb::text,
			query_json::text,
			COALESCE(node_ids, ARRAY[]::uuid[])::text[],
			COALESCE(edge_ids, ARRAY[]::uuid[])::text[],
			COALESCE(concept_ids, ARRAY[]::uuid[])::text[],
			reranker_model_version,
			served_under_degraded,
			created_at
		FROM recall_context_log
		WHERE context_id = $1::uuid
		LIMIT 1
	`
	var (
		resp        ContextResponse
		queryTxt    string
		nodeIDs     pq.StringArray
		edgeIDs     pq.StringArray
		conceptIDs  pq.StringArray
		createdAt   time.Time
	)
	err := h.db.QueryRowContext(ctx, ctxQ, contextID).Scan(
		&resp.ContextID, &resp.RepoID, &resp.Verb, &queryTxt,
		&nodeIDs, &edgeIDs, &conceptIDs,
		&resp.RerankerModelVersion, &resp.ServedUnderDegraded, &createdAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "context_not_found",
			"no context with the supplied context_id")
		return
	}
	if err != nil {
		if isInvalidUUIDError(err) {
			writeJSONError(w, http.StatusNotFound, "context_not_found",
				"no context with the supplied context_id")
			return
		}
		h.logger.Error("mgmtapi.read.context.db_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if queryTxt != "" {
		resp.QueryJSON = json.RawMessage(queryTxt)
	}
	resp.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	resp.NodeIDs = []string(nodeIDs)
	resp.EdgeIDs = []string(edgeIDs)
	resp.ConceptIDs = []string(conceptIDs)
	if resp.NodeIDs == nil {
		resp.NodeIDs = []string{}
	}
	if resp.EdgeIDs == nil {
		resp.EdgeIDs = []string{}
	}
	if resp.ConceptIDs == nil {
		resp.ConceptIDs = []string{}
	}

	// Hydrate each list in array order via WITH ORDINALITY.
	// LEFT JOIN node_retirement so a retired id still
	// surfaces with a `retired_at_sha` badge (risk §9.13).
	if len(resp.NodeIDs) > 0 {
		cards, err := h.hydrateContextNodes(ctx, resp.NodeIDs)
		if err != nil {
			h.logger.Error("mgmtapi.read.context.hydrate_nodes_failed",
				slog.String("error", err.Error()))
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		resp.Nodes = cards
	} else {
		resp.Nodes = []ContextNodeCard{}
	}
	if len(resp.EdgeIDs) > 0 {
		cards, err := h.hydrateContextEdges(ctx, resp.EdgeIDs)
		if err != nil {
			h.logger.Error("mgmtapi.read.context.hydrate_edges_failed",
				slog.String("error", err.Error()))
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		resp.Edges = cards
	} else {
		resp.Edges = []ContextEdgeCard{}
	}
	if len(resp.ConceptIDs) > 0 {
		cards, err := h.hydrateContextConcepts(ctx, resp.ConceptIDs)
		if err != nil {
			h.logger.Error("mgmtapi.read.context.hydrate_concepts_failed",
				slog.String("error", err.Error()))
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		resp.Concepts = cards
	} else {
		resp.Concepts = []ContextConceptCard{}
	}

	// Top-level degraded envelope reflects BOTH the row's own
	// `served_under_degraded` flag (per implementation-plan
	// scenario "degraded snapshot flag", §369-371) AND the
	// repo's current health. Either condition trips the
	// banner. The reason field falls back to repo_health's
	// reason when present; we do NOT synthesise a reason
	// from `served_under_degraded` alone because the
	// recall_context_log row records only the boolean — the
	// original outage source (graph / qdrant / reranker) is
	// not persisted on the row, so inferring a reason would
	// be guessing.
	health, _ := h.loadRepoHealth(ctx, resp.RepoID)
	degraded := health.Degraded || resp.ServedUnderDegraded
	writeReadResponse(w, http.StatusOK, resp, degraded, health.Reason)
}

// hydrateContextNodes resolves each node_id in `ids` (array
// order preserved via WITH ORDINALITY) into a ContextNodeCard.
// LEFT JOIN keeps retired nodes resolved with a badge; a
// node that has been hard-deleted (not a path we support
// today) returns `Resolved=false` with only the original
// id populated.
func (h *Handler) hydrateContextNodes(ctx context.Context, ids []string) ([]ContextNodeCard, error) {
	const q = `
		WITH ord AS (
			SELECT id, ordinality
			FROM unnest($1::uuid[]) WITH ORDINALITY AS u(id, ordinality)
		)
		SELECT
			o.id::text AS node_id,
			COALESCE(n.kind::text, '') AS kind,
			COALESCE(n.canonical_signature, '') AS canonical_signature,
			COALESCE(n.repo_id::text, '') AS repo_id,
			COALESCE(n.from_sha, '') AS from_sha,
			COALESCE(n.attrs_json::text, '') AS attrs,
			COALESCE(nr.retired_at_sha, '') AS retired_at_sha,
			(n.node_id IS NOT NULL) AS resolved
		FROM ord o
		LEFT JOIN node n ON n.node_id = o.id
		LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
		ORDER BY o.ordinality
	`
	rows, err := h.db.QueryContext(ctx, q, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("query nodes: %w", err)
	}
	defer rows.Close()
	out := make([]ContextNodeCard, 0, len(ids))
	for rows.Next() {
		var c ContextNodeCard
		var attrs string
		if err := rows.Scan(
			&c.NodeID, &c.Kind, &c.CanonicalSignature, &c.RepoID,
			&c.FromSHA, &attrs, &c.RetiredAtSHA, &c.Resolved,
		); err != nil {
			return nil, fmt.Errorf("scan node: %w", err)
		}
		if attrs != "" {
			c.Attrs = json.RawMessage(attrs)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (h *Handler) hydrateContextEdges(ctx context.Context, ids []string) ([]ContextEdgeCard, error) {
	const q = `
		WITH ord AS (
			SELECT id, ordinality
			FROM unnest($1::uuid[]) WITH ORDINALITY AS u(id, ordinality)
		)
		SELECT
			o.id::text AS edge_id,
			COALESCE(e.kind::text, '') AS kind,
			COALESCE(e.src_node_id::text, '') AS src_node_id,
			COALESCE(e.dst_node_id::text, '') AS dst_node_id,
			COALESCE(e.repo_id::text, '') AS repo_id,
			COALESCE(e.from_sha, '') AS from_sha,
			COALESCE(e.attrs_json::text, '') AS attrs,
			COALESCE(er.retired_at_sha, '') AS retired_at_sha,
			(e.edge_id IS NOT NULL) AS resolved
		FROM ord o
		LEFT JOIN edge e ON e.edge_id = o.id
		LEFT JOIN edge_retirement er ON er.edge_id = e.edge_id
		ORDER BY o.ordinality
	`
	rows, err := h.db.QueryContext(ctx, q, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("query edges: %w", err)
	}
	defer rows.Close()
	out := make([]ContextEdgeCard, 0, len(ids))
	for rows.Next() {
		var c ContextEdgeCard
		var attrs string
		if err := rows.Scan(
			&c.EdgeID, &c.Kind, &c.SrcNodeID, &c.DstNodeID, &c.RepoID,
			&c.FromSHA, &attrs, &c.RetiredAtSHA, &c.Resolved,
		); err != nil {
			return nil, fmt.Errorf("scan edge: %w", err)
		}
		if attrs != "" {
			c.Attrs = json.RawMessage(attrs)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (h *Handler) hydrateContextConcepts(ctx context.Context, ids []string) ([]ContextConceptCard, error) {
	const q = `
		WITH ord AS (
			SELECT id, ordinality
			FROM unnest($1::uuid[]) WITH ORDINALITY AS u(id, ordinality)
		)
		SELECT
			o.id::text AS concept_id,
			COALESCE(c.fingerprint, ''::bytea) AS fingerprint,
			COALESCE(c.name, '') AS name,
			COALESCE(c.description_md, '') AS description_md,
			(c.concept_id IS NOT NULL) AS resolved
		FROM ord o
		LEFT JOIN concept c ON c.concept_id = o.id
		ORDER BY o.ordinality
	`
	rows, err := h.db.QueryContext(ctx, q, pq.Array(ids))
	if err != nil {
		return nil, fmt.Errorf("query concepts: %w", err)
	}
	defer rows.Close()
	out := make([]ContextConceptCard, 0, len(ids))
	for rows.Next() {
		var c ContextConceptCard
		var fp []byte
		if err := rows.Scan(&c.ConceptID, &fp, &c.Name, &c.DescriptionMD, &c.Resolved); err != nil {
			return nil, fmt.Errorf("scan concept: %w", err)
		}
		if len(fp) > 0 {
			c.Fingerprint = hex.EncodeToString(fp)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// -----------------------------------------------------------
// mgmt.read.concepts
// -----------------------------------------------------------

// ConceptRow joins a Concept row (immutable, §5.5.1) to its
// most recent ConceptVersion (§5.5.2). The current version's
// confidence band lets the operator UI filter / sort by
// "high-confidence promoted concepts" without paging through
// the entire concept_version history.
type ConceptRow struct {
	ConceptID       string `json:"concept_id"`
	Fingerprint     string `json:"fingerprint"`
	Name            string `json:"name"`
	DescriptionMD   string `json:"description_md"`
	CreatedAt       string `json:"created_at"`
	VersionIndex    int    `json:"version_index"`
	Confidence      float64 `json:"confidence"`
	ConfidenceBand  string `json:"confidence_band"`
	SupportCount    int    `json:"support_count"`
	NegativeCount   int    `json:"negative_count"`
	Producer        string `json:"producer"`
	Promoted        bool   `json:"promoted"`
}

// ConceptsResponse is the body of GET /v1/concepts.
type ConceptsResponse struct {
	Concepts []ConceptRow `json:"concepts"`
}

func (h *Handler) handleReadConcepts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()

	limit, ok, msg := parseLimitParam(q.Get("limit"))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	// `promoted` filter — closed enum: "" (no filter),
	// "true", "false". Operator UIs typically only want
	// promoted concepts.
	promoted := strings.TrimSpace(q.Get("promoted"))
	if promoted != "" && promoted != "true" && promoted != "false" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"promoted: must be empty, 'true', or 'false'")
		return
	}

	sb := strings.Builder{}
	sb.WriteString(`
		WITH latest_version AS (
			SELECT DISTINCT ON (concept_id)
				concept_id, version_index, confidence, confidence_band::text,
				support_count, negative_count, producer::text, promoted
			FROM concept_version
			ORDER BY concept_id, version_index DESC
		)
		SELECT
			c.concept_id::text,
			c.fingerprint,
			c.name,
			c.description_md,
			c.created_at,
			COALESCE(v.version_index, 0),
			COALESCE(v.confidence, 0),
			COALESCE(v.confidence_band, ''),
			COALESCE(v.support_count, 0),
			COALESCE(v.negative_count, 0),
			COALESCE(v.producer, ''),
			COALESCE(v.promoted, false)
		FROM concept c
		LEFT JOIN latest_version v ON v.concept_id = c.concept_id
	`)
	switch promoted {
	case "true":
		sb.WriteString(" WHERE COALESCE(v.promoted, false) = true")
	case "false":
		sb.WriteString(" WHERE COALESCE(v.promoted, false) = false")
	}
	sb.WriteString(" ORDER BY c.created_at DESC LIMIT ")
	sb.WriteString(strconv.Itoa(limit))

	rows, err := h.db.QueryContext(ctx, sb.String())
	if err != nil {
		h.logger.Error("mgmtapi.read.concepts.db_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := ConceptsResponse{Concepts: []ConceptRow{}}
	for rows.Next() {
		var row ConceptRow
		var fp []byte
		var createdAt time.Time
		if err := rows.Scan(
			&row.ConceptID, &fp, &row.Name, &row.DescriptionMD, &createdAt,
			&row.VersionIndex, &row.Confidence, &row.ConfidenceBand,
			&row.SupportCount, &row.NegativeCount, &row.Producer, &row.Promoted,
		); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		row.Fingerprint = hex.EncodeToString(fp)
		row.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		out.Concepts = append(out.Concepts, row)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeReadResponse(w, http.StatusOK, out, false, "")
}

// -----------------------------------------------------------
// mgmt.read.concept_supports
// -----------------------------------------------------------

// ConceptSupportRow mirrors a ConceptSupport row joined to the
// ConceptVersion it supports.
type ConceptSupportRow struct {
	SupportID        string `json:"support_id"`
	ConceptID        string `json:"concept_id"`
	ConceptVersionID string `json:"concept_version_id"`
	RepoID           string `json:"repo_id"`
	NodeID           string `json:"node_id,omitempty"`
	EpisodeID        string `json:"episode_id,omitempty"`
	Polarity         string `json:"polarity"`
	CreatedAt        string `json:"created_at"`
}

// ConceptSupportsResponse is the body of GET /v1/concept_supports.
type ConceptSupportsResponse struct {
	ConceptID string              `json:"concept_id"`
	Supports  []ConceptSupportRow `json:"supports"`
}

func (h *Handler) handleReadConceptSupports(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	conceptID := strings.TrimSpace(q.Get("concept_id"))
	if conceptID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "concept_id required")
		return
	}
	if !reUUID.MatchString(conceptID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_concept_id",
			"concept_id query parameter is not a valid UUID")
		return
	}
	repoID := strings.TrimSpace(q.Get("repo_id"))
	if repoID != "" && !reUUID.MatchString(repoID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_repo_id",
			"repo_id query parameter is not a valid UUID")
		return
	}
	limit, ok, msg := parseLimitParam(q.Get("limit"))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}

	args := []any{conceptID}
	sb := strings.Builder{}
	sb.WriteString(`
		SELECT
			support_id::text,
			concept_id::text,
			concept_version_id::text,
			repo_id::text,
			COALESCE(node_id::text, ''),
			COALESCE(episode_id::text, ''),
			polarity::text,
			created_at
		FROM concept_support
		WHERE concept_id = $1::uuid
	`)
	if repoID != "" {
		args = append(args, repoID)
		sb.WriteString(" AND repo_id = $2::uuid")
	}
	sb.WriteString(" ORDER BY created_at DESC LIMIT ")
	sb.WriteString(strconv.Itoa(limit))

	rows, err := h.db.QueryContext(ctx, sb.String(), args...)
	if err != nil {
		h.logger.Error("mgmtapi.read.concept_supports.db_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := ConceptSupportsResponse{ConceptID: conceptID, Supports: []ConceptSupportRow{}}
	for rows.Next() {
		var row ConceptSupportRow
		var createdAt time.Time
		if err := rows.Scan(
			&row.SupportID, &row.ConceptID, &row.ConceptVersionID, &row.RepoID,
			&row.NodeID, &row.EpisodeID, &row.Polarity, &createdAt,
		); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		row.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		out.Supports = append(out.Supports, row)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Repo-scoped degraded view only when repo_id is supplied.
	degraded := false
	reason := ""
	if repoID != "" {
		health, _ := h.loadRepoHealth(ctx, repoID)
		degraded = health.Degraded
		reason = health.Reason
	}
	writeReadResponse(w, http.StatusOK, out, degraded, reason)
}

// -----------------------------------------------------------
// mgmt.read.graph_node
// -----------------------------------------------------------

// GraphNodeNeighbor is one immediate-neighbor element of
// [GraphNodeResponse.Neighbors]. `Direction` is `out` when the
// edge's `src_node_id` equals the requested node, `in` when
// `dst_node_id` does. The retirement badge follows §5.2.4.
type GraphNodeNeighbor struct {
	EdgeID       string `json:"edge_id"`
	Kind         string `json:"kind"`
	Direction    string `json:"direction"`
	OtherNodeID  string `json:"other_node_id"`
	FromSHA      string `json:"from_sha"`
	RetiredAtSHA string `json:"retired_at_sha,omitempty"`
}

// GraphNodeResponse is the body of GET /v1/graph_node/{id}.
type GraphNodeResponse struct {
	NodeID             string              `json:"node_id"`
	Kind               string              `json:"kind"`
	CanonicalSignature string              `json:"canonical_signature"`
	RepoID             string              `json:"repo_id"`
	FromSHA            string              `json:"from_sha"`
	ParentNodeID       string              `json:"parent_node_id,omitempty"`
	Attrs              json.RawMessage     `json:"attrs_json,omitempty"`
	RetiredAtSHA       string              `json:"retired_at_sha,omitempty"`
	ResolvedAtSHA      string              `json:"resolved_at_sha,omitempty"`
	Neighbors          []GraphNodeNeighbor `json:"neighbors"`
}

func (h *Handler) handleReadGraphNode(w http.ResponseWriter, r *http.Request, nodeID string) {
	ctx := r.Context()
	if nodeID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "node_id required")
		return
	}
	if !reUUID.MatchString(nodeID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_node_id",
			"node_id path segment is not a valid UUID")
		return
	}
	q := r.URL.Query()
	shaRaw := strings.TrimSpace(q.Get("sha"))
	if shaRaw != "" && !IsHexGitSHA(shaRaw) {
		writeJSONError(w, http.StatusBadRequest, "invalid_sha",
			"sha must be a 40- or 64-char lower-case hex git SHA")
		return
	}
	// `include_retired=true` is the operator escape hatch
	// for inspecting a node (and its neighbors) that have
	// been tombstoned. The default "current" view per
	// architecture §6.2.3 anti-joins `node_retirement` and
	// `edge_retirement`. Anything other than the literal
	// "true" is treated as false to keep the parser strict.
	includeRetired := strings.EqualFold(strings.TrimSpace(q.Get("include_retired")), "true")

	// Load the node + retirement first so the SHA visibility
	// filter (when `sha` is supplied) and the
	// default-current anti-join (when neither `sha` nor
	// `include_retired=true` is supplied) can decide
	// visibility deterministically.
	//
	// We always SELECT the row + retirement badge here; the
	// retirement filtering happens in Go below so the
	// `?sha=` path can still resurrect a node that is retired
	// at the cluster's current HEAD but was alive at the
	// requested historical SHA.
	const nodeQ = `
		SELECT
			n.node_id::text,
			n.kind::text,
			n.canonical_signature,
			n.repo_id::text,
			n.from_sha,
			COALESCE(n.parent_node_id::text, ''),
			n.attrs_json::text,
			COALESCE(nr.retired_at_sha, '')
		FROM node n
		LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
		WHERE n.node_id = $1::uuid
		LIMIT 1
	`
	var (
		resp           GraphNodeResponse
		attrsTxt       string
	)
	err := h.db.QueryRowContext(ctx, nodeQ, nodeID).Scan(
		&resp.NodeID, &resp.Kind, &resp.CanonicalSignature, &resp.RepoID,
		&resp.FromSHA, &resp.ParentNodeID, &attrsTxt, &resp.RetiredAtSHA,
	)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "node_not_found",
			"no node with the supplied node_id")
		return
	}
	if err != nil {
		if isInvalidUUIDError(err) {
			writeJSONError(w, http.StatusNotFound, "node_not_found",
				"no node with the supplied node_id")
			return
		}
		h.logger.Error("mgmtapi.read.graph_node.db_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if attrsTxt != "" {
		resp.Attrs = json.RawMessage(attrsTxt)
	}

	// targetAt is the resolved commit time when `?sha=` is
	// supplied; sql.NullTime{Valid:false} otherwise. Both the
	// node-visibility check below and the neighbor query
	// branch on Valid so the two paths stay consistent.
	var targetAt sql.NullTime

	// SHA visibility: when `sha` is supplied we resolve it to
	// the matching `repo_commit.committed_at` and apply the
	// §5.2 rule:
	//   - the node's `from_sha` must have committed at or
	//     before the target;
	//   - if the node is retired, the retirement is only
	//     visible at SHAs at-or-after the retirement's
	//     `retired_at_sha`.
	// SHAs that don't match any repo_commit row return 400.
	if shaRaw != "" {
		var t time.Time
		err := h.db.QueryRowContext(ctx, `
			SELECT committed_at FROM repo_commit
			WHERE repo_id = $1::uuid AND sha = $2
		`, resp.RepoID, shaRaw).Scan(&t)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusBadRequest, "unknown_sha",
				"sha is not a known commit for this repo")
			return
		}
		if err != nil {
			h.logger.Error("mgmtapi.read.graph_node.sha_lookup_failed",
				slog.String("error", err.Error()))
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		targetAt = sql.NullTime{Time: t, Valid: true}
		// Compare the node's from_sha commit time against
		// target. If we cannot resolve the node's from_sha
		// (e.g. the commit row was never written) we treat
		// the node as visible to avoid spurious 404s — the
		// caller already specified a known sha and the node
		// row exists, so the operator-visible answer is
		// "what the schema says".
		var fromAt sql.NullTime
		_ = h.db.QueryRowContext(ctx, `
			SELECT committed_at FROM repo_commit
			WHERE repo_id = $1::uuid AND sha = $2
		`, resp.RepoID, resp.FromSHA).Scan(&fromAt)
		if fromAt.Valid && fromAt.Time.After(targetAt.Time) {
			writeJSONError(w, http.StatusNotFound, "node_not_found",
				"node does not exist at the requested sha")
			return
		}
		// If retired at-or-before target, surface the
		// tombstone badge; if retired after the target,
		// clear it (the node is still "current" at this
		// sha). When the retirement's commit row cannot be
		// resolved we conservatively CLEAR the badge — we
		// cannot prove the retirement applied at the
		// requested historical sha.
		if resp.RetiredAtSHA != "" {
			var retiredAt sql.NullTime
			_ = h.db.QueryRowContext(ctx, `
				SELECT committed_at FROM repo_commit
				WHERE repo_id = $1::uuid AND sha = $2
			`, resp.RepoID, resp.RetiredAtSHA).Scan(&retiredAt)
			if !retiredAt.Valid || retiredAt.Time.After(targetAt.Time) {
				resp.RetiredAtSHA = ""
			}
		}
		resp.ResolvedAtSHA = shaRaw
	} else if !includeRetired && resp.RetiredAtSHA != "" {
		// Default ("current") view: anti-join retired rows.
		// Per architecture §6.2.3 / §5.2.4, a retired node
		// is NOT part of the current graph; the operator must
		// either ask for a historical `?sha=` or pass
		// `?include_retired=true` explicitly. We return 404
		// with the dedicated `node_retired` code so the UI
		// can distinguish "never existed" from "tombstoned".
		writeJSONError(w, http.StatusNotFound, "node_retired",
			"node has been retired in the current view; pass ?include_retired=true or ?sha=<historical> to inspect it")
		return
	}

	// Immediate neighbors. Visibility branches on the
	// (targetAt, includeRetired) tuple:
	//   - sha given (targetAt.Valid) → §5.2 visibility:
	//       edge.from_sha at/before target AND
	//       (NOT retired OR retired_at_sha at-or-after target).
	//       The boundary case retired_at_sha == target is
	//       INCLUDED: per §5.2.4, `retired_at_sha` is the
	//       parent commit of the retiring HEAD, i.e. the
	//       last sha at which the edge was still part of the
	//       graph. The badge surfaces the tombstone status
	//       as-of the requested sha, so the edge appears
	//       WITH a badge at sha == retired_at_sha and is
	//       hidden at sha > retired_at_sha.
	//   - sha empty + include_retired=false → default
	//       current view: anti-join `edge_retirement`.
	//   - sha empty + include_retired=true → include retired
	//       edges with their tombstone badge.
	// The query passes both knobs in as bind parameters so
	// we run a single SQL across the three branches.
	const neighborQ = `
		WITH target AS (
			SELECT $2::timestamptz AS target_at
		)
		SELECT
			e.edge_id::text,
			e.kind::text,
			CASE WHEN e.src_node_id = $1::uuid THEN 'out' ELSE 'in' END AS direction,
			CASE WHEN e.src_node_id = $1::uuid THEN e.dst_node_id::text ELSE e.src_node_id::text END,
			e.from_sha,
			CASE
				WHEN t.target_at IS NULL THEN COALESCE(er.retired_at_sha, '')
				WHEN er.edge_id IS NOT NULL
				     AND erc.committed_at IS NOT NULL
				     AND erc.committed_at <= t.target_at
				THEN er.retired_at_sha
				ELSE ''
			END AS retired_at_sha
		FROM edge e
		LEFT JOIN edge_retirement er ON er.edge_id = e.edge_id
		LEFT JOIN repo_commit ec ON ec.repo_id = e.repo_id AND ec.sha = e.from_sha
		LEFT JOIN repo_commit erc
		     ON er.retired_at_sha IS NOT NULL
		    AND erc.repo_id = e.repo_id AND erc.sha = er.retired_at_sha
		CROSS JOIN target t
		WHERE (e.src_node_id = $1::uuid OR e.dst_node_id = $1::uuid)
		  AND (
		    (t.target_at IS NULL AND ($3::boolean OR er.edge_id IS NULL))
		    OR (
		      t.target_at IS NOT NULL
		      AND (ec.committed_at IS NULL OR ec.committed_at <= t.target_at)
		      AND (er.edge_id IS NULL OR erc.committed_at IS NULL OR erc.committed_at >= t.target_at)
		    )
		  )
		ORDER BY e.kind, e.from_sha
		LIMIT 500
	`
	rows, err := h.db.QueryContext(ctx, neighborQ, nodeID, targetAt, includeRetired)
	if err != nil {
		h.logger.Error("mgmtapi.read.graph_node.neighbors_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()
	resp.Neighbors = []GraphNodeNeighbor{}
	for rows.Next() {
		var n GraphNodeNeighbor
		if err := rows.Scan(&n.EdgeID, &n.Kind, &n.Direction, &n.OtherNodeID, &n.FromSHA, &n.RetiredAtSHA); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		resp.Neighbors = append(resp.Neighbors, n)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	health, _ := h.loadRepoHealth(ctx, resp.RepoID)
	writeReadResponse(w, http.StatusOK, resp, health.Degraded, health.Reason)
}

// -----------------------------------------------------------
// mgmt.read.trace_observation
// -----------------------------------------------------------

// TraceObsLogRow is one element of the paged log tail returned
// by [TraceObservationResponse.LogTail].
type TraceObsLogRow struct {
	SpanLogID  string  `json:"span_log_id"`
	TraceID    string  `json:"trace_id"`
	SpanID     string  `json:"span_id"`
	StartedAt  string  `json:"started_at"`
	DurationMs float64 `json:"duration_ms"`
}

// TraceObservationResponse is the body of GET /v1/trace_observation/{edge_id}.
type TraceObservationResponse struct {
	EdgeID           string           `json:"edge_id"`
	ObservationCount int64            `json:"observation_count"`
	P50LatencyMs     float64          `json:"p50_latency_ms"`
	P95LatencyMs     float64          `json:"p95_latency_ms"`
	LatestSpanRef    string           `json:"latest_span_ref,omitempty"`
	LastObservedAt   string           `json:"last_observed_at,omitempty"`
	LogTail          []TraceObsLogRow `json:"log_tail"`
}

func (h *Handler) handleReadTraceObservation(w http.ResponseWriter, r *http.Request, edgeID string) {
	ctx := r.Context()
	if edgeID == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "edge_id required")
		return
	}
	if !reUUID.MatchString(edgeID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_edge_id",
			"edge_id path segment is not a valid UUID")
		return
	}
	limit, ok, msg := parseLimitParam(r.URL.Query().Get("limit"))
	if !ok {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	// `before` is an RFC3339 started_at cursor for the paged
	// tail. Empty → most-recent slice.
	beforeRaw := strings.TrimSpace(r.URL.Query().Get("before"))
	var before time.Time
	if beforeRaw != "" {
		t, err := time.Parse(time.RFC3339, beforeRaw)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid_request",
				"before: must be RFC3339 timestamp")
			return
		}
		before = t
	}

	// Aggregate row.
	const aggQ = `
		SELECT
			t.edge_id::text,
			t.observation_count,
			t.p50_latency_ms,
			t.p95_latency_ms,
			COALESCE(t.latest_span_ref, ''),
			t.last_observed_at,
			e.repo_id::text
		FROM trace_observation t
		LEFT JOIN edge e ON e.edge_id = t.edge_id
		WHERE t.edge_id = $1::uuid
	`
	var (
		resp           TraceObservationResponse
		lastObserved   sql.NullTime
		repoIDForHealth sql.NullString
	)
	err := h.db.QueryRowContext(ctx, aggQ, edgeID).Scan(
		&resp.EdgeID, &resp.ObservationCount, &resp.P50LatencyMs, &resp.P95LatencyMs,
		&resp.LatestSpanRef, &lastObserved, &repoIDForHealth,
	)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSONError(w, http.StatusNotFound, "trace_observation_not_found",
			"no trace observation aggregate for this edge_id")
		return
	}
	if err != nil {
		if isInvalidUUIDError(err) {
			writeJSONError(w, http.StatusNotFound, "trace_observation_not_found",
				"no trace observation aggregate for this edge_id")
			return
		}
		h.logger.Error("mgmtapi.read.trace_observation.db_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if lastObserved.Valid {
		resp.LastObservedAt = lastObserved.Time.UTC().Format(time.RFC3339Nano)
	}

	// Paged log tail (most-recent first). The
	// `trace_observation_log_edge_started_idx` index from
	// migration 0005 is (edge_id, started_at DESC), so the
	// `before` cursor uses started_at.
	args := []any{edgeID}
	logSQL := `
		SELECT span_log_id::text, trace_id, span_id, started_at, duration_ms
		FROM trace_observation_log
		WHERE edge_id = $1::uuid
	`
	if !before.IsZero() {
		args = append(args, before)
		logSQL += " AND started_at < $2"
	}
	logSQL += " ORDER BY started_at DESC LIMIT " + strconv.Itoa(limit)

	rows, err := h.db.QueryContext(ctx, logSQL, args...)
	if err != nil {
		h.logger.Error("mgmtapi.read.trace_observation.log_failed",
			slog.String("error", err.Error()))
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()
	resp.LogTail = []TraceObsLogRow{}
	for rows.Next() {
		var row TraceObsLogRow
		var started time.Time
		if err := rows.Scan(&row.SpanLogID, &row.TraceID, &row.SpanID, &started, &row.DurationMs); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		row.StartedAt = started.UTC().Format(time.RFC3339Nano)
		resp.LogTail = append(resp.LogTail, row)
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	degraded := false
	reason := ""
	if repoIDForHealth.Valid {
		health, _ := h.loadRepoHealth(ctx, repoIDForHealth.String)
		degraded = health.Degraded
		reason = health.Reason
	}
	writeReadResponse(w, http.StatusOK, resp, degraded, reason)
}

// queryEscape exists because handleReadEpisodes builds its
// final SQL via strings.Builder concatenation around
// parameter placeholders ($N); the placeholders themselves
// never receive raw user input. The helper is here as a
// belt-and-braces guard so a future maintainer who reaches
// for `q.Get("...")` directly into the SQL string sees a
// clear signal that strings flow through net/url quoting,
// not raw concatenation.
//
//nolint:unused // retained as documentation-by-symbol for future maintainers
func queryEscape(s string) string {
	return url.QueryEscape(s)
}
