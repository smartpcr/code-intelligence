package mgmtapi

// Stage 7.5: Operator read endpoints.
//
// This file implements every `mgmt.read.*` verb from
// architecture.md §6.2.3 as GET handlers, all wrapping their
// payload in the §6.3 / C22 `degraded` / `degraded_reason`
// envelope so the UI can render a stale-data banner uniformly
// across every read.
//
// Endpoint matrix (verb → URL → required params):
//
//	mgmt.read.repos             GET /v1/repos
//	mgmt.read.commits           GET /v1/commits             ?repo_id=
//	mgmt.read.episodes          GET /v1/episodes            ?since=
//	mgmt.read.observations      GET /v1/observations        ?episode_id=
//	mgmt.read.context           GET /v1/context/{id}
//	mgmt.read.concepts          GET /v1/concepts
//	mgmt.read.concept_supports  GET /v1/concept_supports    ?concept_id=
//	mgmt.read.graph_node        GET /v1/graph_node/{id}
//	mgmt.read.trace_observation GET /v1/trace_observation/{id}
//
// Cross-cutting invariants enforced by every handler in this
// file:
//
//   - Stage 7.5 always serves `degraded=false`. The degraded
//     probe that flips the flag (and populates
//     `degraded_reason`) lands in Stage 8.1; the envelope
//     shape is wired NOW so downstream consumers can rely on
//     it being present from the first deploy.
//   - `mgmt.read.episodes` REQUIRES `?since=` (risk §9.2 -- a
//     full-table scan over the partitioned `episode` table
//     would defeat partition pruning and cost the operator a
//     production-shaped query bill on every UI refresh).
//   - The `current_status` join walks `Episode → latest
//     EpisodeUpdate` per architecture §6.2.3 / tech-spec
//     §8.7.2. The LATERAL form carries an
//     `eu.created_at >= e.created_at` predicate so the
//     planner can prune EpisodeUpdate partitions older than
//     the parent Episode's creation timestamp -- without it
//     the join scans every partition.
//   - Pagination is silently clamped to [readDefaultLimit,
//     readMaxLimit] when the caller supplies `?limit=`;
//     zero, negative, or non-integer values are rejected
//     with 400 so a programmer error surfaces instead of a
//     surprising response shape.
//   - The `?since=` parser accepts BOTH an RFC 3339 timestamp
//     ("2024-01-01T00:00:00Z") and a Go-style duration suffix
//     ("7d", "24h", "30m", "60s"). Bare integers and
//     zero/negative durations are rejected with 400.
//
// Wire shape notes
// ----------------
//
//   - Every response field is JSON `lower_snake_case` to match
//     the rest of the API surface (write verbs in handler.go,
//     feedback.go).
//   - Nullable text columns (`parent_episode_id`,
//     `context_id`, `degraded_reason`, etc.) are typed as
//     omitempty strings; SQL NULL renders as the empty string
//     which omitempty then drops from the JSON object.
//   - UUID columns are emitted as strings (cast `::text` at
//     the DB layer); the JS Date type's lossy UUID parsing is
//     a footgun we avoid by never sending raw uuid objects.
//   - Timestamps are emitted as RFC 3339 (Go's default
//     time.Time JSON encoder).
//
// SHA-pinned graph_node reads
// ---------------------------
//
// The architecture's `mgmt.read.graph_node(node_id, sha?)`
// signature exposes two views of a node's neighborhood:
//
//   - CURRENT view (`?sha=` omitted): the head-state of the
//     repository. The handler always serves the queried node's
//     card -- with a `retired_at_sha` badge when a tombstone
//     exists -- and lists ONLY currently-live neighbors. The
//     neighbor query anti-joins both `edge_retirement` and
//     `node_retirement`, so an edge that points to a retired
//     node, or an edge that has been retired itself, is
//     excluded from the list per architecture §1.3 G5 / §5.2.4
//     ("current = no tombstone").
//
//   - SHA-PINNED view (`?sha=<git-sha>` provided): the
//     historical state of the neighborhood at exactly the
//     requested commit. The handler validates the SHA shape
//     via [IsHexGitSHA], then walks `repo_commit.parent_sha`
//     from the requested SHA back to root via a recursive CTE
//     bound to the node's `repo_id`. Lifecycle decisions for
//     the queried node, its neighbors, and the edges between
//     them are answered by ancestor-set membership of each
//     entity's `from_sha` and `retired_at_sha` -- not by
//     `committed_at` timestamp ordering. (Timestamps are a
//     weak proxy that misbehaves on backdated commits,
//     reverted history, and side branches; ancestor-walk on
//     `parent_sha` is the only correct decision rule.) The
//     normative semantics are pinned by
//     e2e-scenarios.md §195--202:
//       * `mgmt.read.graph_node(N, sha=retired_at_sha)` still
//         returns N (alive at the retirement boundary; the
//         retirement happens at the CHILD commit, not at
//         `retired_at_sha`).
//       * `mgmt.read.graph_node(N, sha=descendant_of_retired_at_sha)`
//         returns N with the `retired_at_sha` badge set.
//
// The lifecycle rules at SHA X:
//
//   - 400 `invalid_sha`        -- malformed SHA shape.
//   - 404 `unknown_sha`        -- X is not in the node's repo's
//                                 commit log.
//   - 404 `node_not_at_sha`    -- the node's `from_sha` is not
//                                 in X's ancestor chain
//                                 (i.e., the node did not yet
//                                 exist at X).
//   - 200 with badge           -- a tombstone exists AND the
//                                 tombstone's `retired_at_sha`
//                                 IS in X's ancestor chain AND
//                                 is NOT equal to X (X is a
//                                 descendant of the boundary,
//                                 so N is dead at X).
//   - 200 with no badge        -- otherwise (alive at X).
//
// Neighbors in SHA-pinned mode apply the same ancestor-set
// rules per edge and per neighbor node: an edge is included
// iff its `from_sha` is in X's ancestors AND its retirement
// (if any) is NOT yet dead at X; the neighbor node is filtered
// with the same predicate.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
)

// ---------------------------------------------------------------
// Route constants
// ---------------------------------------------------------------

// Route prefix constants for the Stage 7.5 read endpoints. The
// `mgmt.read.repos` and `mgmt.read.episodes` verbs reuse the
// existing [RouteRepos] / [RouteEpisodes] prefixes (which
// already serve the Stage 7.1 write verbs); the dispatch by
// HTTP method lives in route() in handler.go.
const (
	// RouteCommits backs `mgmt.read.commits` --
	// GET /v1/commits?repo_id=...
	RouteCommits = "/v1/commits"

	// RouteObservations backs `mgmt.read.observations` --
	// GET /v1/observations?episode_id=...
	RouteObservations = "/v1/observations"

	// RouteConcepts backs `mgmt.read.concepts` --
	// GET /v1/concepts?promoted=...
	RouteConcepts = "/v1/concepts"

	// RouteConceptSupports backs `mgmt.read.concept_supports` --
	// GET /v1/concept_supports?concept_id=...
	RouteConceptSupports = "/v1/concept_supports"

	// RouteContext backs `mgmt.read.context` --
	// GET /v1/context/{context_id}. The trailing slash is
	// implicit in the dispatcher; a bare /v1/context returns
	// 404 because the context_id path segment is mandatory.
	RouteContext = "/v1/context"

	// RouteGraphNode backs `mgmt.read.graph_node` --
	// GET /v1/graph_node/{node_id}. As with [RouteContext],
	// the trailing path segment is mandatory.
	RouteGraphNode = "/v1/graph_node"

	// RouteTraceObservation backs `mgmt.read.trace_observation` --
	// GET /v1/trace_observation/{edge_id}.
	RouteTraceObservation = "/v1/trace_observation"
)

// ---------------------------------------------------------------
// Tunable defaults
// ---------------------------------------------------------------

// Pagination defaults. The 200 / 1000 numbers mirror the
// operator-UI conventions established by the existing
// dashboard scaffolding: 200 rows comfortably fills the
// initial paint of a list page, and 1000 is the hard ceiling
// at which `LIMIT N` stops costing the DB more than an
// uncapped scan would.
const (
	// readDefaultLimit is the `LIMIT` applied when the
	// caller does not supply `?limit=`. Big enough to fill
	// the dashboard's initial paint, small enough that a
	// drive-by `curl` does not pull the entire table.
	readDefaultLimit = 200

	// readMaxLimit is the hard ceiling on caller-supplied
	// `?limit=`. Larger values are silently clamped to this
	// value (NOT rejected -- the §6.2.3 contract phrasing
	// gives the operator a "best-effort" guarantee).
	readMaxLimit = 1000

	// traceTailDefaultLimit / traceTailMaxLimit pin the
	// page size for `mgmt.read.trace_observation`'s log
	// tail. The log is partitioned weekly so a 100-row page
	// is the typical operator query for "show me recent
	// spans on this edge"; the 1000 cap matches the rest of
	// the read surface.
	traceTailDefaultLimit = 100
	traceTailMaxLimit     = 1000

	// graphNeighborLimit caps the per-direction neighbor
	// fanout returned by `mgmt.read.graph_node`. A hot
	// supernode (e.g. a popular utility method) can have
	// thousands of inbound `static_calls` edges; we cap the
	// per-direction emission so the operator UI's "node
	// card" render never balloons the response.
	graphNeighborLimit = 200
)

// ---------------------------------------------------------------
// Response wire shapes
// ---------------------------------------------------------------

// RepoCard is the per-row shape emitted by mgmt.read.repos.
// Mirrors architecture.md §5.6 Repo plus the latest ingest
// job joined as `latest_ingest_*`.
type RepoCard struct {
	RepoID         string    `json:"repo_id"`
	URL            string    `json:"url"`
	DefaultBranch  string    `json:"default_branch"`
	CurrentHeadSHA string    `json:"current_head_sha"`
	CreatedAt      time.Time `json:"created_at"`

	// Latest ingest job, joined via LATERAL. Empty strings
	// when the repo has no ingest_jobs row yet (e.g. a
	// register that hasn't been picked up by the Repo
	// Indexer worker).
	LatestIngestJobID     string    `json:"latest_ingest_job_id,omitempty"`
	LatestIngestStatus    string    `json:"latest_ingest_status,omitempty"`
	LatestIngestMode      string    `json:"latest_ingest_mode,omitempty"`
	LatestIngestUpdatedAt time.Time `json:"latest_ingest_updated_at,omitempty"`
}

// ListReposResponse is the wire shape of GET /v1/repos.
type ListReposResponse struct {
	Repos []RepoCard `json:"repos"`
}

// CommitCard is the per-row shape emitted by mgmt.read.commits.
type CommitCard struct {
	SHA         string    `json:"sha"`
	ParentSHA   string    `json:"parent_sha,omitempty"`
	CommittedAt time.Time `json:"committed_at"`
	IndexStatus string    `json:"index_status"`
}

// ListCommitsResponse is the wire shape of GET /v1/commits.
type ListCommitsResponse struct {
	RepoID  string       `json:"repo_id"`
	Commits []CommitCard `json:"commits"`
}

// EpisodeCard is the per-row shape emitted by mgmt.read.episodes.
// `CurrentStatus` is the latest EpisodeUpdate.new_outcome
// joined per architecture §6.2.3; the original `Outcome` column
// stays unchanged (G3 -- the parent row is never mutated).
type EpisodeCard struct {
	EpisodeID       string    `json:"episode_id"`
	EpisodeGroupID  string    `json:"episode_group_id"`
	RepoID          string    `json:"repo_id"`
	SessionID       string    `json:"session_id"`
	TraceID         string    `json:"trace_id"`
	Kind            string    `json:"kind"`
	Outcome         string    `json:"outcome"`
	ParentEpisodeID string    `json:"parent_episode_id,omitempty"`
	ContextID       string    `json:"context_id,omitempty"`
	Degraded        bool      `json:"degraded"`
	DegradedReason  string    `json:"degraded_reason,omitempty"`
	CreatedAt       time.Time `json:"created_at"`

	// CurrentStatus is the most recent EpisodeUpdate.new_outcome.
	// Equal to Outcome when no update exists. Per the §6.2.3
	// `current_status` definition.
	CurrentStatus          string    `json:"current_status"`
	CurrentStatusUpdatedAt time.Time `json:"current_status_updated_at,omitempty"`
}

// ListEpisodesResponse is the wire shape of GET /v1/episodes.
type ListEpisodesResponse struct {
	Episodes []EpisodeCard `json:"episodes"`
}

// ObservationCard is the per-row shape emitted by mgmt.read.observations.
// Exactly one of NodeID / EdgeID / ConceptID /
// DegradedRecallContextID is set per the
// `observation_exactly_one_target_chk` schema constraint.
type ObservationCard struct {
	ObservationID            string    `json:"observation_id"`
	Role                     string    `json:"role"`
	NodeID                   string    `json:"node_id,omitempty"`
	EdgeID                   string    `json:"edge_id,omitempty"`
	ConceptID                string    `json:"concept_id,omitempty"`
	DegradedRecallContextID  string    `json:"degraded_recall_context_id,omitempty"`
	Weight                   float64   `json:"weight"`
	CreatedAt                time.Time `json:"created_at"`
}

// ListObservationsResponse is the wire shape of GET /v1/observations.
type ListObservationsResponse struct {
	EpisodeID    string            `json:"episode_id"`
	Observations []ObservationCard `json:"observations"`
}

// ContextNodeCard surfaces a Node referenced by a
// RecallContextLog row. `RetiredAtSHA` is non-empty iff the
// Node has a `node_retirement` tombstone, per the §9.13 risk
// scenario the operator UI surfaces as a "retired" badge.
type ContextNodeCard struct {
	Ordinal             int    `json:"ordinal"`
	NodeID              string `json:"node_id"`
	Kind                string `json:"kind,omitempty"`
	CanonicalSignature  string `json:"canonical_signature,omitempty"`
	RetiredAtSHA        string `json:"retired_at_sha,omitempty"`
	// Missing is true when the node_id no longer exists in
	// the `node` table at all (defensive against a
	// not-yet-encountered cascade-delete edge case; the
	// schema currently has ON DELETE RESTRICT on Node FKs
	// so this should never trip in steady state).
	Missing bool `json:"missing,omitempty"`
}

// ContextEdgeCard surfaces an Edge referenced by a
// RecallContextLog row. Same retirement / missing semantics as
// [ContextNodeCard].
type ContextEdgeCard struct {
	Ordinal       int    `json:"ordinal"`
	EdgeID        string `json:"edge_id"`
	Kind          string `json:"kind,omitempty"`
	SrcNodeID     string `json:"src_node_id,omitempty"`
	DstNodeID     string `json:"dst_node_id,omitempty"`
	RetiredAtSHA  string `json:"retired_at_sha,omitempty"`
	Missing       bool   `json:"missing,omitempty"`
}

// ContextConceptCard surfaces a Concept referenced by a
// RecallContextLog row. Concepts have no retirement table
// (they are append-only at the concept layer per G6); the
// `Missing` flag is the only defensive signal.
type ContextConceptCard struct {
	Ordinal      int    `json:"ordinal"`
	ConceptID    string `json:"concept_id"`
	Name         string `json:"name,omitempty"`
	Missing      bool   `json:"missing,omitempty"`
}

// ContextResponse is the wire shape of GET /v1/context/{id}.
type ContextResponse struct {
	ContextID            string               `json:"context_id"`
	RepoID               string               `json:"repo_id"`
	Verb                 string               `json:"verb"`
	QueryJSON            json.RawMessage      `json:"query_json"`
	RerankerModelVersion string               `json:"reranker_model_version"`
	ServedUnderDegraded  bool                 `json:"served_under_degraded"`
	CreatedAt            time.Time            `json:"created_at"`
	Nodes                []ContextNodeCard    `json:"nodes"`
	Edges                []ContextEdgeCard    `json:"edges"`
	Concepts             []ContextConceptCard `json:"concepts"`
}

// ConceptCard is the per-row shape emitted by mgmt.read.concepts.
// Embeds the latest ConceptVersion (per architecture §5.5.2)
// in-line via LATERAL join.
type ConceptCard struct {
	ConceptID     string    `json:"concept_id"`
	Name          string    `json:"name"`
	DescriptionMD string    `json:"description_md"`
	CreatedAt     time.Time `json:"created_at"`

	// Latest ConceptVersion fields (omitempty when no
	// version exists, which should never happen in steady
	// state because the Consolidator writes v=0 with every
	// new Concept).
	LatestVersionIndex      int       `json:"latest_version_index,omitempty"`
	LatestConfidence        float64   `json:"latest_confidence,omitempty"`
	LatestConfidenceBand    string    `json:"latest_confidence_band,omitempty"`
	LatestSupportCount      int       `json:"latest_support_count,omitempty"`
	LatestNegativeCount     int       `json:"latest_negative_count,omitempty"`
	LatestPromoted          bool      `json:"latest_promoted"`
	LatestVersionCreatedAt  time.Time `json:"latest_version_created_at,omitempty"`
}

// ListConceptsResponse is the wire shape of GET /v1/concepts.
type ListConceptsResponse struct {
	Concepts []ConceptCard `json:"concepts"`
}

// ConceptSupportCard is the per-row shape emitted by mgmt.read.concept_supports.
type ConceptSupportCard struct {
	SupportID         string    `json:"support_id"`
	ConceptID         string    `json:"concept_id"`
	ConceptVersionID  string    `json:"concept_version_id"`
	RepoID            string    `json:"repo_id"`
	NodeID            string    `json:"node_id,omitempty"`
	EpisodeID         string    `json:"episode_id,omitempty"`
	Polarity          string    `json:"polarity"`
	CreatedAt         time.Time `json:"created_at"`
}

// ListConceptSupportsResponse is the wire shape of GET /v1/concept_supports.
type ListConceptSupportsResponse struct {
	ConceptID string               `json:"concept_id"`
	Supports  []ConceptSupportCard `json:"supports"`
}

// GraphNodeNeighbor is one row of the inbound/outbound edge
// list rendered alongside a graph_node response.
type GraphNodeNeighbor struct {
	EdgeID                  string `json:"edge_id"`
	EdgeKind                string `json:"edge_kind"`
	NeighborNodeID          string `json:"neighbor_node_id"`
	NeighborKind            string `json:"neighbor_kind,omitempty"`
	NeighborSignature       string `json:"neighbor_canonical_signature,omitempty"`
	EdgeRetiredAtSHA        string `json:"edge_retired_at_sha,omitempty"`
	NeighborMissing         bool   `json:"neighbor_missing,omitempty"`
}

// GraphNodeResponse is the wire shape of GET /v1/graph_node/{id}.
type GraphNodeResponse struct {
	NodeID              string              `json:"node_id"`
	RepoID              string              `json:"repo_id"`
	Kind                string              `json:"kind"`
	CanonicalSignature  string              `json:"canonical_signature"`
	FromSHA             string              `json:"from_sha"`
	ParentNodeID        string              `json:"parent_node_id,omitempty"`
	AttrsJSON           json.RawMessage     `json:"attrs_json"`
	RetiredAtSHA        string              `json:"retired_at_sha,omitempty"`
	OutgoingEdges       []GraphNodeNeighbor `json:"outgoing_edges"`
	IncomingEdges       []GraphNodeNeighbor `json:"incoming_edges"`
}

// TraceObservationTailRow is one row of the paged span log
// tail returned alongside `mgmt.read.trace_observation`.
type TraceObservationTailRow struct {
	SpanLogID   string    `json:"span_log_id"`
	TraceID     string    `json:"trace_id"`
	SpanID      string    `json:"span_id"`
	StartedAt   time.Time `json:"started_at"`
	DurationMS  float64   `json:"duration_ms"`
}

// TraceObservationResponse is the wire shape of
// GET /v1/trace_observation/{edge_id}.
type TraceObservationResponse struct {
	EdgeID            string                     `json:"edge_id"`
	ObservationCount  int64                      `json:"observation_count"`
	P50LatencyMS      float64                    `json:"p50_latency_ms"`
	P95LatencyMS      float64                    `json:"p95_latency_ms"`
	LatestSpanRef     string                     `json:"latest_span_ref,omitempty"`
	LastObservedAt    time.Time                  `json:"last_observed_at,omitempty"`
	Tail              []TraceObservationTailRow  `json:"tail"`
	NextOffset        int                        `json:"next_offset,omitempty"`
}

// ---------------------------------------------------------------
// Closed-set whitelists for ?outcome_in / ?kind_in
// ---------------------------------------------------------------

// readAllowedOutcomes mirrors the `outcome` ENUM in
// migrations/0001_enums.sql.
var readAllowedOutcomes = map[string]struct{}{
	"success":         {},
	"failure":         {},
	"refused":         {},
	"degraded":        {},
	"human_corrected": {},
}

// readAllowedEpisodeKinds mirrors the `episode_kind` ENUM.
var readAllowedEpisodeKinds = map[string]struct{}{
	"agent":              {},
	"feedback":           {},
	"synthetic_positive": {},
}

// ---------------------------------------------------------------
// Shared helpers (query-param parsing)
// ---------------------------------------------------------------

// parseLimitParam reads `?limit=` from `r`. Returns `def` when
// the param is absent; clamps to `max` when present and
// positive; returns a non-nil error (400-shaped) on non-integer
// or non-positive values.
func parseLimitParam(r *http.Request, def, max int) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("limit must be a positive integer; got %q", raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("limit must be a positive integer; got %d", n)
	}
	if n > max {
		n = max
	}
	return n, nil
}

// parseOffsetParam reads `?offset=` from `r`. Returns 0 when
// absent; rejects negative values with a 400-shaped error.
// Non-integer values likewise yield an error.
func parseOffsetParam(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("offset")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("offset must be a non-negative integer; got %q", raw)
	}
	if n < 0 {
		return 0, fmt.Errorf("offset must be a non-negative integer; got %d", n)
	}
	return n, nil
}

// parseSinceParam reads `?since=` from `r`. Accepts either
// an RFC 3339 absolute timestamp ("2024-01-01T00:00:00Z") or
// a duration suffix (`Nd`, `Nh`, `Nm`, `Ns`). When the input
// is a duration, returns `now - duration`. `now()` is captured
// once per call so a single request never sees clock drift
// mid-parse.
//
// Returns (zero, false, err) on a malformed input. Returns
// (zero, false, nil) when the param is absent. Returns
// (time, true, nil) on success.
func parseSinceParam(r *http.Request, now time.Time) (time.Time, bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("since"))
	if raw == "" {
		return time.Time{}, false, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true, nil
	}
	d, err := parseSinceDuration(raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf(
			"since must be an RFC 3339 timestamp or a duration like 7d/24h/30m/60s; got %q", raw)
	}
	if d <= 0 {
		return time.Time{}, false, fmt.Errorf(
			"since duration must be positive; got %q", raw)
	}
	return now.Add(-d), true, nil
}

// parseSinceDuration parses a duration suffix accepted by
// `?since=`. Supports `Nd` (days), `Nh` (hours), `Nm`
// (minutes), `Ns` (seconds). Bare integers / unknown suffixes
// return an error.
func parseSinceDuration(raw string) (time.Duration, error) {
	if len(raw) < 2 {
		return 0, fmt.Errorf("too short")
	}
	unit := raw[len(raw)-1]
	numStr := raw[:len(raw)-1]
	n, err := strconv.Atoi(numStr)
	if err != nil {
		return 0, err
	}
	switch unit {
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	case 'm':
		return time.Duration(n) * time.Minute, nil
	case 's':
		return time.Duration(n) * time.Second, nil
	default:
		return 0, fmt.Errorf("unknown unit %q", string(unit))
	}
}

// parseUUIDParam returns the value of query parameter `name`,
// trimmed. Returns ("", nil) when the param is absent and
// `required` is false. Returns ("", err) when the param is
// either required-and-absent or present-but-malformed.
func parseUUIDParam(r *http.Request, name string, required bool) (string, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		if required {
			return "", fmt.Errorf("%s is required", name)
		}
		return "", nil
	}
	if !reUUID.MatchString(raw) {
		return "", fmt.Errorf("%s is not a valid UUID", name)
	}
	return raw, nil
}

// parseBoolParam reads a query parameter as a boolean. Returns
// (false, false, nil) when absent; (parsed, true, nil) on
// success; (false, false, err) on malformed input.
func parseBoolParam(r *http.Request, name string) (val, present bool, err error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return false, false, nil
	}
	b, perr := strconv.ParseBool(raw)
	if perr != nil {
		return false, false, fmt.Errorf("%s must be a boolean; got %q", name, raw)
	}
	return b, true, nil
}

// parseEnumCSVParam splits `?name=foo,bar` into a slice and
// validates each member against `allowed`. Returns nil + nil
// when the param is absent (the caller treats the resulting
// nil slice as "no filter"). Returns an error on a malformed
// value or an unknown enum member.
func parseEnumCSVParam(r *http.Request, name string, allowed map[string]struct{}) ([]string, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := strings.TrimSpace(p)
		if v == "" {
			continue
		}
		if _, ok := allowed[v]; !ok {
			return nil, fmt.Errorf("%s contains unknown value %q", name, v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s is empty after trimming", name)
	}
	return out, nil
}

// extractTrailingPathID extracts the single trailing segment
// of `path` after `prefix`. Returns ok=false if the path does
// not start with `prefix+"/"`, if the rest is empty, or if
// the rest contains a further `/`. The id is NOT validated as
// a UUID here -- that is the handler's responsibility so 400
// (invalid id) vs 404 (unknown route) attribution stays
// clean.
func extractTrailingPathID(prefix, path string) (string, bool) {
	rest := strings.TrimPrefix(path, prefix+"/")
	if rest == path {
		return "", false
	}
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// writeReadResponse wraps `payload` in a DegradedEnvelope with
// the §6.3 / C22 degraded fields stamped to false and emits
// the JSON. Stage 7.5 always serves `degraded=false`; Stage
// 8.1 will introduce a probe that flips the flag based on
// `repo_health` and per-verb fault-injection.
func writeReadResponse[T any](w http.ResponseWriter, payload T) {
	writeJSONResponse(w, http.StatusOK, DegradedEnvelope[T]{Payload: payload})
}

// logReadError emits a structured ERROR-level log for a
// failed read query. Keeps the per-handler boilerplate small.
func (h *Handler) logReadError(op string, r *http.Request, err error) {
	subject, _ := SubjectFromContext(r.Context())
	h.logger.Error("mgmtapi."+op+".db_failed",
		slog.String("op", op),
		slog.String("path", r.URL.Path),
		slog.String("query", redactRawQuery(r.URL)),
		slog.String("subject", subject),
		slog.String("error", err.Error()),
	)
}

// redactRawQuery returns the request URL's query string with
// any sensitive values stripped. Currently a passthrough --
// none of the Stage 7.5 query parameters carry secrets -- but
// kept as a hook so a future param like `?include_secret=`
// can be redacted without touching every call site.
func redactRawQuery(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.RawQuery
}

// ---------------------------------------------------------------
// mgmt.read.repos
// ---------------------------------------------------------------

// handleListRepos serves GET /v1/repos. Optional `?repo_id=`
// filters to a single Repo; optional `?limit=` clamps the
// page size.
func (h *Handler) handleListRepos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repoID, err := parseUUIDParam(r, "repo_id", false)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	limit, err := parseLimitParam(r, readDefaultLimit, readMaxLimit)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	const q = `
		SELECT r.repo_id::text, r.url, r.default_branch, r.current_head_sha,
		       r.created_at,
		       COALESCE(j.job_id::text, ''),
		       COALESCE(j.status::text, ''),
		       COALESCE(j.mode::text, ''),
		       j.updated_at
		FROM repo r
		LEFT JOIN LATERAL (
		    SELECT job_id, status, mode, updated_at
		    FROM ingest_jobs
		    WHERE repo_id = r.repo_id
		    ORDER BY updated_at DESC, job_id DESC
		    LIMIT 1
		) j ON true
		WHERE ($1::text = '' OR r.repo_id = $1::uuid)
		ORDER BY r.created_at DESC, r.repo_id DESC
		LIMIT $2
	`
	rows, err := h.db.QueryContext(ctx, q, repoID, limit)
	if err != nil {
		h.logReadError("read.repos", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := make([]RepoCard, 0, 16)
	for rows.Next() {
		var c RepoCard
		var lastUpdated sql.NullTime
		if err := rows.Scan(
			&c.RepoID, &c.URL, &c.DefaultBranch, &c.CurrentHeadSHA, &c.CreatedAt,
			&c.LatestIngestJobID, &c.LatestIngestStatus, &c.LatestIngestMode, &lastUpdated,
		); err != nil {
			h.logReadError("read.repos", r, err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if lastUpdated.Valid {
			c.LatestIngestUpdatedAt = lastUpdated.Time
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		h.logReadError("read.repos", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeReadResponse(w, ListReposResponse{Repos: out})
}

// ---------------------------------------------------------------
// mgmt.read.commits
// ---------------------------------------------------------------

// handleListCommits serves GET /v1/commits?repo_id=...
// Required: `repo_id`. Optional: `since`, `limit`.
func (h *Handler) handleListCommits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repoID, err := parseUUIDParam(r, "repo_id", true)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	since, sinceSet, err := parseSinceParam(r, h.clock())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	limit, err := parseLimitParam(r, readDefaultLimit, readMaxLimit)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	const q = `
		SELECT sha, COALESCE(parent_sha, ''), committed_at, index_status
		FROM repo_commit
		WHERE repo_id = $1::uuid
		  AND ($3::boolean = false OR committed_at >= $2::timestamptz)
		ORDER BY committed_at DESC, sha
		LIMIT $4
	`
	rows, err := h.db.QueryContext(ctx, q, repoID, since, sinceSet, limit)
	if err != nil {
		h.logReadError("read.commits", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := make([]CommitCard, 0, 16)
	for rows.Next() {
		var c CommitCard
		if err := rows.Scan(&c.SHA, &c.ParentSHA, &c.CommittedAt, &c.IndexStatus); err != nil {
			h.logReadError("read.commits", r, err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		h.logReadError("read.commits", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeReadResponse(w, ListCommitsResponse{RepoID: repoID, Commits: out})
}

// ---------------------------------------------------------------
// mgmt.read.episodes
// ---------------------------------------------------------------

// handleListEpisodes serves GET /v1/episodes?since=...
// Required: `since` (risk §9.2 -- partition pruning). Optional:
// `repo_id`, `outcome_in`, `kind_in`, `limit`. Each row carries
// a `current_status` field joined from the latest EpisodeUpdate.
func (h *Handler) handleListEpisodes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	since, sinceSet, err := parseSinceParam(r, h.clock())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !sinceSet {
		writeJSONError(w, http.StatusBadRequest, "since_required",
			"since query parameter is required (risk §9.2: partition pruning)")
		return
	}
	repoID, err := parseUUIDParam(r, "repo_id", false)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	outcomes, err := parseEnumCSVParam(r, "outcome_in", readAllowedOutcomes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	kinds, err := parseEnumCSVParam(r, "kind_in", readAllowedEpisodeKinds)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	limit, err := parseLimitParam(r, readDefaultLimit, readMaxLimit)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	const q = `
		SELECT e.episode_id::text, e.episode_group_id::text, e.repo_id::text,
		       e.session_id, e.trace_id, e.kind::text, e.outcome::text,
		       COALESCE(e.parent_episode_id::text, ''),
		       COALESCE(e.context_id::text, ''),
		       e.degraded, COALESCE(e.degraded_reason::text, ''),
		       e.created_at,
		       COALESCE(eu.new_outcome::text, e.outcome::text),
		       eu.created_at
		FROM episode e
		LEFT JOIN LATERAL (
		    SELECT new_outcome, created_at, update_id
		    FROM episode_update
		    WHERE episode_id = e.episode_id
		      AND created_at >= e.created_at
		    ORDER BY created_at DESC, update_id DESC
		    LIMIT 1
		) eu ON true
		WHERE e.created_at >= $1::timestamptz
		  AND ($2::text = '' OR e.repo_id = $2::uuid)
		  AND (cardinality($3::text[]) = 0 OR e.outcome::text = ANY($3::text[]))
		  AND (cardinality($4::text[]) = 0 OR e.kind::text    = ANY($4::text[]))
		ORDER BY e.created_at DESC, e.episode_id DESC
		LIMIT $5
	`
	rows, err := h.db.QueryContext(ctx, q,
		since, repoID,
		pq.Array(outcomes), pq.Array(kinds),
		limit,
	)
	if err != nil {
		h.logReadError("read.episodes", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := make([]EpisodeCard, 0, 16)
	for rows.Next() {
		var c EpisodeCard
		var statusAt sql.NullTime
		if err := rows.Scan(
			&c.EpisodeID, &c.EpisodeGroupID, &c.RepoID,
			&c.SessionID, &c.TraceID, &c.Kind, &c.Outcome,
			&c.ParentEpisodeID, &c.ContextID,
			&c.Degraded, &c.DegradedReason,
			&c.CreatedAt, &c.CurrentStatus, &statusAt,
		); err != nil {
			h.logReadError("read.episodes", r, err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if statusAt.Valid {
			c.CurrentStatusUpdatedAt = statusAt.Time
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		h.logReadError("read.episodes", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeReadResponse(w, ListEpisodesResponse{Episodes: out})
}

// ---------------------------------------------------------------
// mgmt.read.observations
// ---------------------------------------------------------------

// handleListObservations serves GET /v1/observations?episode_id=...
// Required: `episode_id`. Loads the parent Episode's
// `created_at` first so the partitioned `observation` table
// scan can prune to partitions ≥ that timestamp.
func (h *Handler) handleListObservations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	episodeID, err := parseUUIDParam(r, "episode_id", true)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Step 1: locate the parent Episode's created_at. Without
	// this anchor the observation read scans every partition.
	const loadParent = `
		SELECT created_at FROM episode WHERE episode_id = $1::uuid LIMIT 1
	`
	var parentCreatedAt time.Time
	switch err := h.db.QueryRowContext(ctx, loadParent, episodeID).Scan(&parentCreatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		writeJSONError(w, http.StatusNotFound, "episode_not_found",
			"no episode with the supplied episode_id")
		return
	case err != nil:
		if isInvalidUUIDError(err) {
			writeJSONError(w, http.StatusNotFound, "episode_not_found",
				"no episode with the supplied episode_id")
			return
		}
		h.logReadError("read.observations", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}

	// Step 2: the actual observation read, with the
	// partition-prune predicate.
	const q = `
		SELECT observation_id::text, role::text,
		       COALESCE(node_id::text, ''),
		       COALESCE(edge_id::text, ''),
		       COALESCE(concept_id::text, ''),
		       COALESCE(degraded_recall_context_id::text, ''),
		       weight, created_at
		FROM observation
		WHERE episode_id = $1::uuid
		  AND created_at >= $2::timestamptz
		ORDER BY created_at ASC, observation_id ASC
	`
	rows, err := h.db.QueryContext(ctx, q, episodeID, parentCreatedAt)
	if err != nil {
		h.logReadError("read.observations", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := make([]ObservationCard, 0, 16)
	for rows.Next() {
		var o ObservationCard
		if err := rows.Scan(
			&o.ObservationID, &o.Role,
			&o.NodeID, &o.EdgeID, &o.ConceptID, &o.DegradedRecallContextID,
			&o.Weight, &o.CreatedAt,
		); err != nil {
			h.logReadError("read.observations", r, err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		h.logReadError("read.observations", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeReadResponse(w, ListObservationsResponse{
		EpisodeID:    episodeID,
		Observations: out,
	})
}

// ---------------------------------------------------------------
// mgmt.read.context
// ---------------------------------------------------------------

// handleReadContext serves GET /v1/context/{context_id}. The
// path id is required and validated as a UUID.
func (h *Handler) handleReadContext(w http.ResponseWriter, r *http.Request, rawCtxID string) {
	ctx := r.Context()
	if !reUUID.MatchString(rawCtxID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_context_id",
			"context_id path segment is not a valid UUID")
		return
	}

	// Step 1: load the RecallContextLog row. LIMIT 2 is the
	// corruption guard -- the (context_id, created_at)
	// composite PK should yield at most one row in steady
	// state; a second row signals a writer bug we want to
	// surface as a 500 instead of silently picking one.
	const loadLog = `
		SELECT context_id::text, repo_id::text, verb::text,
		       query_json::text, reranker_model_version,
		       served_under_degraded, created_at,
		       node_ids::text[], edge_ids::text[], concept_ids::text[]
		FROM recall_context_log
		WHERE context_id = $1::uuid
		LIMIT 2
	`
	rows, err := h.db.QueryContext(ctx, loadLog, rawCtxID)
	if err != nil {
		if isInvalidUUIDError(err) {
			writeJSONError(w, http.StatusNotFound, "context_not_found",
				"no recall context with the supplied id")
			return
		}
		h.logReadError("read.context", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	var (
		found bool
		resp  ContextResponse
		nodeIDs, edgeIDs, conceptIDs pq.StringArray
		queryJSONStr string
	)
	for rows.Next() {
		if found {
			h.logger.Error("mgmtapi.read.context.duplicate_rows",
				slog.String("op", "read.context"),
				slog.String("context_id", rawCtxID),
			)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		found = true
		if err := rows.Scan(
			&resp.ContextID, &resp.RepoID, &resp.Verb,
			&queryJSONStr, &resp.RerankerModelVersion,
			&resp.ServedUnderDegraded, &resp.CreatedAt,
			&nodeIDs, &edgeIDs, &conceptIDs,
		); err != nil {
			h.logReadError("read.context", r, err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
	}
	if err := rows.Err(); err != nil {
		h.logReadError("read.context", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if !found {
		writeJSONError(w, http.StatusNotFound, "context_not_found",
			"no recall context with the supplied id")
		return
	}

	// Release the connection before issuing the dereference
	// queries; sql.DB pools may be small and we don't want
	// to hold a slot across three follow-up round trips.
	rows.Close()

	resp.QueryJSON = json.RawMessage(queryJSONStr)
	if len(resp.QueryJSON) == 0 {
		resp.QueryJSON = json.RawMessage("null")
	}

	nodes, err := h.fetchContextNodes(ctx, nodeIDs)
	if err != nil {
		h.logReadError("read.context", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	edges, err := h.fetchContextEdges(ctx, edgeIDs)
	if err != nil {
		h.logReadError("read.context", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	concepts, err := h.fetchContextConcepts(ctx, conceptIDs)
	if err != nil {
		h.logReadError("read.context", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	resp.Nodes = nodes
	resp.Edges = edges
	resp.Concepts = concepts
	writeReadResponse(w, resp)
}

// fetchContextNodes dereferences `node_ids[]` from a
// RecallContextLog row, surfacing the retirement tombstone via
// LEFT JOIN to `node_retirement`. The `unnest WITH ORDINALITY`
// + LEFT JOIN preserves array order AND tolerates a missing
// (cascade-deleted) Node id by leaving the joined columns
// NULL.
func (h *Handler) fetchContextNodes(ctx context.Context, ids pq.StringArray) ([]ContextNodeCard, error) {
	if len(ids) == 0 {
		return []ContextNodeCard{}, nil
	}
	const q = `
		SELECT u.idx,
		       u.node_id::text,
		       COALESCE(n.kind::text, ''),
		       COALESCE(n.canonical_signature, ''),
		       COALESCE(nr.retired_at_sha, ''),
		       (n.node_id IS NULL) AS missing
		FROM unnest($1::uuid[]) WITH ORDINALITY AS u(node_id, idx)
		LEFT JOIN node n ON n.node_id = u.node_id
		LEFT JOIN node_retirement nr ON nr.node_id = u.node_id
		ORDER BY u.idx
	`
	rows, err := h.db.QueryContext(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch context nodes: %w", err)
	}
	defer rows.Close()
	out := make([]ContextNodeCard, 0, len(ids))
	for rows.Next() {
		var c ContextNodeCard
		if err := rows.Scan(&c.Ordinal, &c.NodeID, &c.Kind, &c.CanonicalSignature, &c.RetiredAtSHA, &c.Missing); err != nil {
			return nil, fmt.Errorf("scan context node: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// fetchContextEdges dereferences `edge_ids[]` analogously to
// fetchContextNodes; surfaces edge retirement via LEFT JOIN
// to `edge_retirement` and resolves the src/dst node ids in
// the same round trip.
func (h *Handler) fetchContextEdges(ctx context.Context, ids pq.StringArray) ([]ContextEdgeCard, error) {
	if len(ids) == 0 {
		return []ContextEdgeCard{}, nil
	}
	const q = `
		SELECT u.idx,
		       u.edge_id::text,
		       COALESCE(e.kind::text, ''),
		       COALESCE(e.src_node_id::text, ''),
		       COALESCE(e.dst_node_id::text, ''),
		       COALESCE(er.retired_at_sha, ''),
		       (e.edge_id IS NULL) AS missing
		FROM unnest($1::uuid[]) WITH ORDINALITY AS u(edge_id, idx)
		LEFT JOIN edge e ON e.edge_id = u.edge_id
		LEFT JOIN edge_retirement er ON er.edge_id = u.edge_id
		ORDER BY u.idx
	`
	rows, err := h.db.QueryContext(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch context edges: %w", err)
	}
	defer rows.Close()
	out := make([]ContextEdgeCard, 0, len(ids))
	for rows.Next() {
		var c ContextEdgeCard
		if err := rows.Scan(&c.Ordinal, &c.EdgeID, &c.Kind, &c.SrcNodeID, &c.DstNodeID, &c.RetiredAtSHA, &c.Missing); err != nil {
			return nil, fmt.Errorf("scan context edge: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// fetchContextConcepts dereferences `concept_ids[]`. Concepts
// have no retirement table -- the append-only concept layer
// (G6) does not tombstone -- so only `missing` is surfaced.
func (h *Handler) fetchContextConcepts(ctx context.Context, ids pq.StringArray) ([]ContextConceptCard, error) {
	if len(ids) == 0 {
		return []ContextConceptCard{}, nil
	}
	const q = `
		SELECT u.idx,
		       u.concept_id::text,
		       COALESCE(c.name, ''),
		       (c.concept_id IS NULL) AS missing
		FROM unnest($1::uuid[]) WITH ORDINALITY AS u(concept_id, idx)
		LEFT JOIN concept c ON c.concept_id = u.concept_id
		ORDER BY u.idx
	`
	rows, err := h.db.QueryContext(ctx, q, ids)
	if err != nil {
		return nil, fmt.Errorf("fetch context concepts: %w", err)
	}
	defer rows.Close()
	out := make([]ContextConceptCard, 0, len(ids))
	for rows.Next() {
		var c ContextConceptCard
		if err := rows.Scan(&c.Ordinal, &c.ConceptID, &c.Name, &c.Missing); err != nil {
			return nil, fmt.Errorf("scan context concept: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------
// mgmt.read.concepts
// ---------------------------------------------------------------

// handleListConcepts serves GET /v1/concepts. Optional
// `?promoted=true|false` filters by the latest version's
// promoted flag; optional `?limit=` clamps the page size.
func (h *Handler) handleListConcepts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	promoted, promotedSet, err := parseBoolParam(r, "promoted")
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	limit, err := parseLimitParam(r, readDefaultLimit, readMaxLimit)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// $1 = promoted_set, $2 = promoted_value, $3 = limit.
	// The filter is opt-in (when promoted_set=false the
	// promoted predicate evaluates to true).
	const q = `
		SELECT c.concept_id::text, c.name, c.description_md, c.created_at,
		       COALESCE(cv.version_index, 0),
		       COALESCE(cv.confidence, 0),
		       COALESCE(cv.confidence_band::text, ''),
		       COALESCE(cv.support_count, 0),
		       COALESCE(cv.negative_count, 0),
		       COALESCE(cv.promoted, false),
		       cv.created_at
		FROM concept c
		LEFT JOIN LATERAL (
		    SELECT version_index, confidence, confidence_band,
		           support_count, negative_count, promoted, created_at
		    FROM concept_version
		    WHERE concept_id = c.concept_id
		    ORDER BY version_index DESC
		    LIMIT 1
		) cv ON true
		WHERE ($1::boolean = false OR COALESCE(cv.promoted, false) = $2::boolean)
		ORDER BY c.created_at DESC, c.concept_id DESC
		LIMIT $3
	`
	rows, err := h.db.QueryContext(ctx, q, promotedSet, promoted, limit)
	if err != nil {
		h.logReadError("read.concepts", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := make([]ConceptCard, 0, 16)
	for rows.Next() {
		var c ConceptCard
		var verCreatedAt sql.NullTime
		if err := rows.Scan(
			&c.ConceptID, &c.Name, &c.DescriptionMD, &c.CreatedAt,
			&c.LatestVersionIndex, &c.LatestConfidence, &c.LatestConfidenceBand,
			&c.LatestSupportCount, &c.LatestNegativeCount, &c.LatestPromoted,
			&verCreatedAt,
		); err != nil {
			h.logReadError("read.concepts", r, err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if verCreatedAt.Valid {
			c.LatestVersionCreatedAt = verCreatedAt.Time
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		h.logReadError("read.concepts", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeReadResponse(w, ListConceptsResponse{Concepts: out})
}

// ---------------------------------------------------------------
// mgmt.read.concept_supports
// ---------------------------------------------------------------

// handleListConceptSupports serves GET /v1/concept_supports?concept_id=...
// Required: `concept_id`. Optional: `repo_id`, `limit`.
func (h *Handler) handleListConceptSupports(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	conceptID, err := parseUUIDParam(r, "concept_id", true)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	repoID, err := parseUUIDParam(r, "repo_id", false)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	limit, err := parseLimitParam(r, readDefaultLimit, readMaxLimit)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	const q = `
		SELECT support_id::text, concept_id::text, concept_version_id::text,
		       repo_id::text,
		       COALESCE(node_id::text, ''),
		       COALESCE(episode_id::text, ''),
		       polarity::text, created_at
		FROM concept_support
		WHERE concept_id = $1::uuid
		  AND ($2::text = '' OR repo_id = $2::uuid)
		ORDER BY created_at DESC, support_id DESC
		LIMIT $3
	`
	rows, err := h.db.QueryContext(ctx, q, conceptID, repoID, limit)
	if err != nil {
		h.logReadError("read.concept_supports", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()

	out := make([]ConceptSupportCard, 0, 16)
	for rows.Next() {
		var c ConceptSupportCard
		if err := rows.Scan(
			&c.SupportID, &c.ConceptID, &c.ConceptVersionID,
			&c.RepoID, &c.NodeID, &c.EpisodeID,
			&c.Polarity, &c.CreatedAt,
		); err != nil {
			h.logReadError("read.concept_supports", r, err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		h.logReadError("read.concept_supports", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	writeReadResponse(w, ListConceptSupportsResponse{
		ConceptID: conceptID,
		Supports:  out,
	})
}

// ---------------------------------------------------------------
// mgmt.read.graph_node
// ---------------------------------------------------------------

// handleReadGraphNode serves GET /v1/graph_node/{node_id}.
// Supports the optional `?sha=<git-sha>` query parameter per
// architecture §6.2 `(node_id, sha?)`. See the file header
// for the full ancestor-walk semantics.
func (h *Handler) handleReadGraphNode(w http.ResponseWriter, r *http.Request, rawNodeID string) {
	ctx := r.Context()
	if !reUUID.MatchString(rawNodeID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_node_id",
			"node_id path segment is not a valid UUID")
		return
	}
	rawSHA := strings.TrimSpace(r.URL.Query().Get("sha"))
	if rawSHA != "" && !IsHexGitSHA(rawSHA) {
		writeJSONError(w, http.StatusBadRequest, "invalid_sha",
			"sha must be a 40- or 64-char lower-case hex git SHA")
		return
	}

	// Step 1: load the node row + its retirement (if any). The
	// returned `repoID` anchors the ancestor walk in step 2 to
	// the node's own repo.
	const loadNode = `
		SELECT n.repo_id::text, n.kind::text, n.canonical_signature, n.from_sha,
		       COALESCE(n.parent_node_id::text, ''),
		       n.attrs_json::text,
		       COALESCE(nr.retired_at_sha, '')
		FROM node n
		LEFT JOIN node_retirement nr ON nr.node_id = n.node_id
		WHERE n.node_id = $1::uuid
	`
	var resp GraphNodeResponse
	resp.NodeID = rawNodeID
	var attrsJSON string
	var retiredAtSHA string
	switch err := h.db.QueryRowContext(ctx, loadNode, rawNodeID).Scan(
		&resp.RepoID, &resp.Kind, &resp.CanonicalSignature, &resp.FromSHA,
		&resp.ParentNodeID, &attrsJSON, &retiredAtSHA,
	); {
	case errors.Is(err, sql.ErrNoRows):
		writeJSONError(w, http.StatusNotFound, "node_not_found",
			"no node with the supplied id")
		return
	case err != nil:
		if isInvalidUUIDError(err) {
			writeJSONError(w, http.StatusNotFound, "node_not_found",
				"no node with the supplied id")
			return
		}
		h.logReadError("read.graph_node", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if attrsJSON == "" {
		resp.AttrsJSON = json.RawMessage("{}")
	} else {
		resp.AttrsJSON = json.RawMessage(attrsJSON)
	}

	// Step 2: resolve lifecycle state at the requested view.
	scope := neighborScope{currentView: true}
	if rawSHA != "" {
		state, err := h.resolveGraphNodeShaState(ctx, resp.RepoID, rawSHA, resp.FromSHA, retiredAtSHA)
		if err != nil {
			h.logReadError("read.graph_node", r, err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		if !state.targetKnown {
			writeJSONError(w, http.StatusNotFound, "unknown_sha",
				"sha is not present in this repo's commit log")
			return
		}
		if !state.fromInAncestors {
			writeJSONError(w, http.StatusNotFound, "node_not_at_sha",
				"node did not exist at the requested sha")
			return
		}
		// Tombstoned at target iff the retirement boundary is
		// in X's ancestor chain AND is strictly an ancestor of
		// X (retired_at_sha == X means the node was alive at
		// X -- the retirement happens at the child commit).
		if retiredAtSHA != "" && state.retireInAncestors && retiredAtSHA != rawSHA {
			resp.RetiredAtSHA = retiredAtSHA
		}
		scope = neighborScope{
			currentView:   false,
			repoID:        resp.RepoID,
			targetSHA:     rawSHA,
		}
	} else {
		// Current view: keep the existing tombstone-badge
		// behaviour (e2e §200 expects the queried node card
		// to surface its retirement, even though it is
		// excluded from neighbor lists per G5).
		resp.RetiredAtSHA = retiredAtSHA
	}

	out, err := h.fetchNodeNeighbors(ctx, rawNodeID, "outgoing", scope)
	if err != nil {
		h.logReadError("read.graph_node", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	in, err := h.fetchNodeNeighbors(ctx, rawNodeID, "incoming", scope)
	if err != nil {
		h.logReadError("read.graph_node", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	resp.OutgoingEdges = out
	resp.IncomingEdges = in
	writeReadResponse(w, resp)
}

// graphNodeShaState captures the booleans needed to decide a
// node's lifecycle at a SHA-pinned view of mgmt.read.graph_node.
type graphNodeShaState struct {
	// targetKnown is true iff the requested sha is in the
	// node's repo's commit log (i.e. the anchor of the
	// recursive CTE found a row).
	targetKnown bool
	// fromInAncestors is true iff the node's `from_sha`
	// (the SHA at which it first appeared) is in the
	// ancestor chain of the requested sha.
	fromInAncestors bool
	// retireInAncestors is true iff the node has a tombstone
	// AND its `retired_at_sha` is in the ancestor chain of
	// the requested sha. The caller still has to compare
	// `retired_at_sha == requested_sha` to distinguish
	// "alive at the retirement boundary" from "dead at a
	// descendant".
	retireInAncestors bool
}

// resolveGraphNodeShaState evaluates the three ancestor-set
// membership questions for the SHA-pinned graph_node view in
// a single round-trip: target known, node.from_sha in
// ancestors, retired_at_sha in ancestors. `retiredAtSHA` may
// be the empty string when the node has no tombstone -- the
// `retire_in_ancestors` result is then always false because
// the comparand never matches any anchor row.
func (h *Handler) resolveGraphNodeShaState(
	ctx context.Context,
	repoID, targetSHA, fromSHA, retiredAtSHA string,
) (graphNodeShaState, error) {
	const q = `
		WITH RECURSIVE ancestors (sha, parent_sha) AS (
			SELECT sha, parent_sha
			FROM repo_commit
			WHERE repo_id = $1::uuid AND sha = $2
			UNION ALL
			SELECT rc.sha, rc.parent_sha
			FROM repo_commit rc
			JOIN ancestors a ON rc.repo_id = $1::uuid AND rc.sha = a.parent_sha
		)
		SELECT
			EXISTS (SELECT 1 FROM ancestors)                          AS target_known,
			EXISTS (SELECT 1 FROM ancestors WHERE sha = $3)           AS from_in_ancestors,
			EXISTS (SELECT 1 FROM ancestors WHERE sha = $4 AND $4 != '') AS retire_in_ancestors
	`
	var state graphNodeShaState
	if err := h.db.QueryRowContext(ctx, q, repoID, targetSHA, fromSHA, retiredAtSHA).
		Scan(&state.targetKnown, &state.fromInAncestors, &state.retireInAncestors); err != nil {
		return graphNodeShaState{}, fmt.Errorf("resolve sha state: %w", err)
	}
	return state, nil
}

// neighborScope tells [Handler.fetchNodeNeighbors] which view
// to render. `currentView` selects the head-state anti-join
// query (`er.edge_id IS NULL AND nr.node_id IS NULL`); a
// false value with `repoID` + `targetSHA` selects the
// SHA-pinned ancestor-walk query.
type neighborScope struct {
	currentView bool
	repoID      string
	targetSHA   string
}

// fetchNodeNeighbors returns the per-direction neighbor list
// for `mgmt.read.graph_node`. `direction` MUST be "outgoing"
// (edges sourced AT the node) or "incoming" (edges landing
// AT the node). Capped at [graphNeighborLimit] rows per call.
//
// Behaviour matrix (see file header for full semantics):
//
//   - `scope.currentView == true`  -> anti-join both
//     `edge_retirement` and `node_retirement`, so only
//     currently-live edges to currently-live neighbors
//     appear.
//   - `scope.currentView == false` -> walk
//     `repo_commit.parent_sha` from `scope.targetSHA`,
//     then per-edge filter on ancestor-set membership of
//     each edge's `from_sha` / `retired_at_sha` AND each
//     neighbor node's `from_sha` /
//     `node_retirement.retired_at_sha`.
func (h *Handler) fetchNodeNeighbors(
	ctx context.Context, nodeID, direction string, scope neighborScope,
) ([]GraphNodeNeighbor, error) {
	var srcCol, dstCol string
	switch direction {
	case "outgoing":
		srcCol, dstCol = "src_node_id", "dst_node_id"
	case "incoming":
		srcCol, dstCol = "dst_node_id", "src_node_id"
	default:
		return nil, fmt.Errorf("unknown direction %q", direction)
	}
	if scope.currentView {
		return h.fetchCurrentNeighbors(ctx, nodeID, srcCol, dstCol)
	}
	return h.fetchShaPinnedNeighbors(ctx, nodeID, srcCol, dstCol, scope.repoID, scope.targetSHA)
}

// fetchCurrentNeighbors serves the head-state neighbor list.
// Retired edges AND edges pointing at retired neighbors are
// excluded via anti-join per architecture §1.3 G5 / §5.2.4
// ("current = no tombstone").
func (h *Handler) fetchCurrentNeighbors(
	ctx context.Context, nodeID, srcCol, dstCol string,
) ([]GraphNodeNeighbor, error) {
	q := fmt.Sprintf(`
		SELECT e.edge_id::text, e.kind::text,
		       e.%[2]s::text,
		       COALESCE(neighbor.canonical_signature, ''),
		       COALESCE(neighbor.kind::text, ''),
		       ''::text AS edge_retired_at_sha,
		       (neighbor.node_id IS NULL) AS missing
		FROM edge e
		LEFT JOIN node neighbor ON neighbor.node_id = e.%[2]s
		LEFT JOIN edge_retirement er ON er.edge_id = e.edge_id
		LEFT JOIN node_retirement nr ON nr.node_id = e.%[2]s
		WHERE e.%[1]s = $1::uuid
		  AND er.edge_id IS NULL
		  AND nr.node_id IS NULL
		ORDER BY e.kind::text ASC, e.edge_id ASC
		LIMIT $2
	`, srcCol, dstCol)
	rows, err := h.db.QueryContext(ctx, q, nodeID, graphNeighborLimit)
	if err != nil {
		return nil, fmt.Errorf("fetch current neighbors: %w", err)
	}
	defer rows.Close()
	return scanGraphNodeNeighbors(rows)
}

// fetchShaPinnedNeighbors serves the SHA-pinned neighbor
// list. Edges and neighbor nodes are filtered by ancestor-set
// membership against a recursive CTE walking
// `repo_commit.parent_sha` from `targetSHA` back to root.
//
// The two retirement predicates accept rows whose retirement
// is NOT in the ancestor chain OR whose `retired_at_sha`
// equals the requested SHA (the entity is alive at the
// retirement boundary; the retirement is registered against
// the parent of the new HEAD per G5).
func (h *Handler) fetchShaPinnedNeighbors(
	ctx context.Context, nodeID, srcCol, dstCol, repoID, targetSHA string,
) ([]GraphNodeNeighbor, error) {
	q := fmt.Sprintf(`
		WITH RECURSIVE ancestors (sha, parent_sha) AS (
			SELECT sha, parent_sha
			FROM repo_commit
			WHERE repo_id = $2::uuid AND sha = $3
			UNION ALL
			SELECT rc.sha, rc.parent_sha
			FROM repo_commit rc
			JOIN ancestors a ON rc.repo_id = $2::uuid AND rc.sha = a.parent_sha
		)
		SELECT e.edge_id::text, e.kind::text,
		       e.%[2]s::text,
		       COALESCE(neighbor.canonical_signature, ''),
		       COALESCE(neighbor.kind::text, ''),
		       ''::text AS edge_retired_at_sha,
		       (neighbor.node_id IS NULL) AS missing
		FROM edge e
		LEFT JOIN node neighbor ON neighbor.node_id = e.%[2]s
		LEFT JOIN edge_retirement er ON er.edge_id = e.edge_id
		LEFT JOIN node_retirement nr ON nr.node_id = e.%[2]s
		WHERE e.%[1]s = $1::uuid
		  AND EXISTS (SELECT 1 FROM ancestors WHERE sha = e.from_sha)
		  AND (
		    er.edge_id IS NULL
		    OR er.retired_at_sha = $3
		    OR NOT EXISTS (SELECT 1 FROM ancestors WHERE sha = er.retired_at_sha)
		  )
		  AND (
		    neighbor.node_id IS NULL
		    OR EXISTS (SELECT 1 FROM ancestors WHERE sha = neighbor.from_sha)
		  )
		  AND (
		    nr.node_id IS NULL
		    OR nr.retired_at_sha = $3
		    OR NOT EXISTS (SELECT 1 FROM ancestors WHERE sha = nr.retired_at_sha)
		  )
		ORDER BY e.kind::text ASC, e.edge_id ASC
		LIMIT $4
	`, srcCol, dstCol)
	rows, err := h.db.QueryContext(ctx, q, nodeID, repoID, targetSHA, graphNeighborLimit)
	if err != nil {
		return nil, fmt.Errorf("fetch sha-pinned neighbors: %w", err)
	}
	defer rows.Close()
	return scanGraphNodeNeighbors(rows)
}

// scanGraphNodeNeighbors decodes the seven-column row shape
// produced by [Handler.fetchCurrentNeighbors] and
// [Handler.fetchShaPinnedNeighbors].
func scanGraphNodeNeighbors(rows *sql.Rows) ([]GraphNodeNeighbor, error) {
	out := make([]GraphNodeNeighbor, 0, 16)
	for rows.Next() {
		var n GraphNodeNeighbor
		if err := rows.Scan(
			&n.EdgeID, &n.EdgeKind, &n.NeighborNodeID,
			&n.NeighborSignature, &n.NeighborKind,
			&n.EdgeRetiredAtSHA, &n.NeighborMissing,
		); err != nil {
			return nil, fmt.Errorf("scan neighbor: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------
// mgmt.read.trace_observation
// ---------------------------------------------------------------

// handleReadTraceObservation serves GET
// /v1/trace_observation/{edge_id}. Returns the aggregate row
// plus a paged tail of the span log. `?limit=` and `?offset=`
// page the tail; `?since=` windows the tail to spans started
// after the cutoff (RFC 3339 or Nd/Nh/Nm/Ns duration).
func (h *Handler) handleReadTraceObservation(w http.ResponseWriter, r *http.Request, rawEdgeID string) {
	ctx := r.Context()
	if !reUUID.MatchString(rawEdgeID) {
		writeJSONError(w, http.StatusBadRequest, "invalid_edge_id",
			"edge_id path segment is not a valid UUID")
		return
	}
	limit, err := parseLimitParam(r, traceTailDefaultLimit, traceTailMaxLimit)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	offset, err := parseOffsetParam(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	since, sinceSet, err := parseSinceParam(r, h.clock())
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Step 1: the aggregate. 404 if the edge has no
	// trace_observation row at all (which is the canonical
	// signal that we've never observed a span on it).
	const loadAgg = `
		SELECT observation_count, p50_latency_ms, p95_latency_ms,
		       COALESCE(latest_span_ref, ''), last_observed_at
		FROM trace_observation
		WHERE edge_id = $1::uuid
	`
	resp := TraceObservationResponse{EdgeID: rawEdgeID, Tail: []TraceObservationTailRow{}}
	var lastObserved sql.NullTime
	switch err := h.db.QueryRowContext(ctx, loadAgg, rawEdgeID).Scan(
		&resp.ObservationCount, &resp.P50LatencyMS, &resp.P95LatencyMS,
		&resp.LatestSpanRef, &lastObserved,
	); {
	case errors.Is(err, sql.ErrNoRows):
		writeJSONError(w, http.StatusNotFound, "trace_observation_not_found",
			"no trace_observation row for the supplied edge_id")
		return
	case err != nil:
		if isInvalidUUIDError(err) {
			writeJSONError(w, http.StatusNotFound, "trace_observation_not_found",
				"no trace_observation row for the supplied edge_id")
			return
		}
		h.logReadError("read.trace_observation", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if lastObserved.Valid {
		resp.LastObservedAt = lastObserved.Time
	}

	// Step 2: the tail. ORDER BY (started_at DESC,
	// span_log_id DESC) keeps the pagination deterministic
	// across pages with identical timestamps. Fetch one
	// extra row so we can emit a NextOffset hint when more
	// rows exist past the requested page.
	const loadTail = `
		SELECT span_log_id::text, trace_id, span_id, started_at, duration_ms
		FROM trace_observation_log
		WHERE edge_id = $1::uuid
		  AND ($3::boolean = false OR started_at >= $2::timestamptz)
		ORDER BY started_at DESC, span_log_id DESC
		LIMIT $4 OFFSET $5
	`
	rows, err := h.db.QueryContext(ctx, loadTail,
		rawEdgeID, since, sinceSet, limit+1, offset,
	)
	if err != nil {
		h.logReadError("read.trace_observation", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	defer rows.Close()
	tail := make([]TraceObservationTailRow, 0, limit)
	for rows.Next() {
		var t TraceObservationTailRow
		if err := rows.Scan(&t.SpanLogID, &t.TraceID, &t.SpanID, &t.StartedAt, &t.DurationMS); err != nil {
			h.logReadError("read.trace_observation", r, err)
			writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
			return
		}
		tail = append(tail, t)
	}
	if err := rows.Err(); err != nil {
		h.logReadError("read.trace_observation", r, err)
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "internal error")
		return
	}
	if len(tail) > limit {
		tail = tail[:limit]
		resp.NextOffset = offset + limit
	}
	resp.Tail = tail
	writeReadResponse(w, resp)
}
