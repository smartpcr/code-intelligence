package rerankertrainer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/lib/pq"
)

// LabelledPair is one row in the training set the trainer
// consumes. The schema is intentionally rich enough to feed a
// cross-encoder reranker: every column the §6.4 step-2 contract
// names (positive / negative Episode classification, the
// RecallContextLog seed set, the per-observation role + weight,
// the corrected action -- if any) is carried as a structured
// field.
//
// The struct is JSON-serialisable so the production trainer
// (which lives off-process in a Python sidecar per
// architecture §3.6) can consume it without re-parsing the
// database -- the Go binary owns the schema knowledge and
// hands the trainer a flat, denormalised pair list.
type LabelledPair struct {
	// EpisodeID is the originating Episode's UUID
	// (textually-rendered for JSON portability). Used as
	// the dedup key inside the per-actor cap and as a
	// contributor to the version fingerprint.
	EpisodeID string `json:"episode_id"`

	// EpisodeKind is one of {"agent", "feedback",
	// "synthetic_positive"} per `episode_kind` ENUM.
	// Carried so the trainer can branch on provenance
	// (e.g. only learn the correction signal from synthetic
	// positives).
	EpisodeKind string `json:"episode_kind"`

	// Outcome is the Episode's terminal outcome at the
	// moment the pair was extracted (one of
	// {"success", "failure", "refused", "degraded",
	// "human_corrected"}). For "agent" positives this is
	// "success"; for "synthetic_positive" rows this is
	// "success" by construction (the parent's correction is
	// the supervised label). For negatives this is
	// "failure" / "degraded" / "human_corrected" depending
	// on the source.
	Outcome string `json:"outcome"`

	// CreatedAt is the Episode row's `created_at`
	// timestamp. Used by the trainer for time-decay
	// weighting (production trainer concern) and by the
	// 90-day window pre-filter at the SQL layer.
	CreatedAt time.Time `json:"created_at"`

	// RepoID is the originating repo (UUID text). Carried
	// so the trainer can stratify train/eval splits per
	// repo if the configured strategy demands it.
	RepoID string `json:"repo_id"`

	// ContextID is the seed `recall_context_log.context_id`
	// the Episode consumed (UUID text). Empty when the
	// originating Episode had a NULL context_id (a feedback
	// Episode -- but those are not emitted as pairs in v1).
	ContextID string `json:"context_id,omitempty"`

	// QueryJSON is the raw `recall_context_log.query_json`
	// the originating recall consumed. Carried as
	// json.RawMessage so the trainer can parse it without
	// the Go layer needing to know the query shape (it is
	// caller-controlled).
	QueryJSON json.RawMessage `json:"query_json,omitempty"`

	// RecallQuery is the natural-language query string the
	// originating recall consumed, extracted from
	// `query_json.query` during context hydration. This is
	// the SAME string `agent.recall` callers send and the
	// SAME string `/rank` receives in its `query` field, so
	// train-time and recall-time `_project_query` outputs
	// match by construction (closes iter-5 review item 1+2:
	// previously the sidecar received empty `recall_query`
	// and fell back to seed IDs while ranking used the user
	// query — that train/recall surface drift is now gone).
	// Empty when the originating Episode had no associated
	// RecallContextLog (degraded path) OR when `query_json`
	// did not carry a top-level `query` string field.
	RecallQuery string `json:"recall_query,omitempty"`

	// SeedNodeIDs is the ordered list of node UUIDs the
	// recall returned (the "what was retrieved" surface
	// the reranker re-orders). One slice item per node id
	// in `recall_context_log.node_ids`. Empty when there
	// was no associated RecallContextLog (degraded path).
	SeedNodeIDs []string `json:"seed_node_ids,omitempty"`

	// SeedEdgeIDs / SeedConceptIDs mirror SeedNodeIDs for
	// the other two seed kinds. Populated from the
	// corresponding `recall_context_log.edge_ids` /
	// `concept_ids` arrays.
	SeedEdgeIDs    []string `json:"seed_edge_ids,omitempty"`
	SeedConceptIDs []string `json:"seed_concept_ids,omitempty"`

	// Observations is the per-Episode observation set:
	// one entry per `observation` row attributed to this
	// Episode, carrying role + weight + the target id.
	// Empty when no observations were recorded (a
	// well-formed positive Episode always has at least one
	// observation; absence is a data-shape signal worth
	// keeping visible to the trainer).
	Observations []LabelledObservation `json:"observations,omitempty"`

	// Action is the originating Episode's `action` JSONB
	// column verbatim. The trainer treats this as the
	// "what the agent did" half of the supervised pair.
	Action json.RawMessage `json:"action,omitempty"`

	// CorrectedAction is the operator's intended action
	// (the `episode.corrected_action` JSONB column).
	// Populated when this pair's Episode is a
	// "synthetic_positive" OR a "human_corrected" parent
	// (whose corrected_action lives on the *feedback*
	// child, not on the parent itself -- the trainer-side
	// merge is the consumer's responsibility).
	CorrectedAction json.RawMessage `json:"corrected_action,omitempty"`

	// CorrectionActor is the `episode_update.actor` enum
	// value of the most-recent `new_outcome='human_corrected'`
	// EpisodeUpdate against this Episode (negatives) OR
	// against this Episode's parent (positives derived
	// from a synthetic_positive). Empty for pairs whose
	// originating Episode never received a human-correction
	// update (e.g. an outright failure or a
	// non-correction-derived success). Used by the
	// per-actor cap classifier to charge BOTH halves of
	// the correction (the demoted parent AND the promoted
	// synthetic_positive) against the same operator
	// budget.
	CorrectionActor string `json:"correction_actor,omitempty"`

	// CorrectionUpdateAt is the `created_at` of that
	// EpisodeUpdate. Zero when CorrectionActor is empty.
	// Used by the cap's sliding-window predicate so a
	// 90-day-old correction does not consume cap budget
	// that should be reserved for the recent hour.
	CorrectionUpdateAt time.Time `json:"correction_update_at,omitempty"`
}

// LabelledObservation is one row in LabelledPair.Observations.
// Schema mirrors `observation` (modulo the single-target
// invariant -- exactly one of NodeID / EdgeID / ConceptID is
// populated per the §8.7.4 check constraint).
type LabelledObservation struct {
	Role      string  `json:"role"`
	Weight    float64 `json:"weight"`
	NodeID    string  `json:"node_id,omitempty"`
	EdgeID    string  `json:"edge_id,omitempty"`
	ConceptID string  `json:"concept_id,omitempty"`
}

// PullOpts configures the pair-pulling pass.
type PullOpts struct {
	// Now anchors the trailing-window filter. Tests inject
	// a fixed value; production passes time.Now().
	Now time.Time

	// Window is the trailing-window length used to filter
	// Episodes (impl-plan §6.4 line 1119: "last 90 days").
	// Applies to both the kind='agent' rows AND the
	// kind='synthetic_positive' rows (and their parent
	// negatives) -- old corrections do not consume
	// retraining budget.
	Window time.Duration

	// ActorCapPerWindow is the per-actor cap on
	// correction-derived pairs within ActorCapWindow. A
	// zero or negative value disables the cap (every
	// candidate is kept).
	ActorCapPerWindow int

	// ActorCapWindow is the sliding-window length the cap
	// applies over. A zero or negative value falls back to
	// 1h.
	ActorCapWindow time.Duration
}

// PullResult is the output of PullPairs.
type PullResult struct {
	// Positives is the labelled positive set, already
	// filtered through the per-actor cap.
	Positives []LabelledPair

	// Negatives is the labelled negative set, already
	// filtered through the per-actor cap.
	Negatives []LabelledPair

	// CappedActors maps `episode_update.actor` enum value
	// -> dropped-pair count. Used by the binary's metrics
	// to emit `reranker_capped_actor_total{actor=...}`.
	CappedActors map[string]uint64
}

// PullPairs scans the partitioned Episode tree (joined to
// recall_context_log and observation) and returns the labelled
// training set. The selection rules implement the §6.4 step-2
// contract:
//
//	Positives:
//	  - Every `kind='synthetic_positive'` Episode whose
//	    `created_at >= now - opts.Window` (impl-plan §6.4
//	    line 1119: "last 90 days"). Each row's
//	    correction_actor is attributed via a LATERAL lookup
//	    of the originating `episode_update` so the §9.4
//	    per-actor cap can fire on operator-noisy positive
//	    corrections too.
//	  - `kind='agent' AND outcome='success'` Episodes inside
//	    the trailing `opts.Window`.
//
//	Negatives:
//	  - `kind='agent' AND outcome IN ('failure','degraded')`
//	    inside the trailing window.
//	  - The PARENT Episodes of every synthetic_positive
//	    whose CHILD's `created_at >= now - opts.Window`
//	    (joined via `synthesized_from_parent_episode_id`).
//	    These are the "pre-correction" record the trainer
//	    learns to demote.
//
// The per-actor cap (opts.ActorCapPerWindow) is applied to
// BOTH the correction-attributed positives AND the
// correction-parent negatives, then the per-actor drop counts
// are merged across both buckets. The cap channel is the §9.4
// poisoning vector; a noisy operator's corrections drive both
// the synthetic_positive AND the demoted parent so both halves
// must consume the same per-operator budget. Plain
// failure/degraded Episodes (no correction attribution) are
// NOT capped.
//
// PullPairs treats every database error as fatal -- the
// trainer is run on a long cadence (24h) and a partial scan
// would produce a misleading version fingerprint. The caller
// (Service.Tick) wraps the error and emits an `errors_total`
// bump.
func PullPairs(ctx context.Context, db *sql.DB, opts PullOpts) (PullResult, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	if opts.Window <= 0 {
		opts.Window = 90 * 24 * time.Hour
	}
	windowStart := opts.Now.Add(-opts.Window)

	positives, err := scanPositives(ctx, db, windowStart)
	if err != nil {
		return PullResult{}, fmt.Errorf("rerankertrainer: scan positives: %w", err)
	}
	negatives, err := scanNegatives(ctx, db, windowStart)
	if err != nil {
		return PullResult{}, fmt.Errorf("rerankertrainer: scan negatives: %w", err)
	}

	// Hydrate the seed context + observations for every
	// pair. Done in batched id passes so we do not issue
	// one query per pair.
	if err := hydrateContext(ctx, db, positives); err != nil {
		return PullResult{}, fmt.Errorf("rerankertrainer: hydrate positives: %w", err)
	}
	if err := hydrateContext(ctx, db, negatives); err != nil {
		return PullResult{}, fmt.Errorf("rerankertrainer: hydrate negatives: %w", err)
	}
	if err := hydrateObservations(ctx, db, positives); err != nil {
		return PullResult{}, fmt.Errorf("rerankertrainer: observations positives: %w", err)
	}
	if err := hydrateObservations(ctx, db, negatives); err != nil {
		return PullResult{}, fmt.Errorf("rerankertrainer: observations negatives: %w", err)
	}

	// COMBINED actor budget: the §9.4 mitigation caps an
	// operator's TOTAL correction contribution per window,
	// not their per-bucket contribution. A noisy operator who
	// flips 100 Episodes inside the cap window puts 100
	// synthetic_positive rows AND 100 parent-of-positive
	// rows into the trainer's input, and BOTH halves are
	// attributable to that operator. Applying the cap
	// separately to positives and negatives would let an
	// operator keep up to N entries in each bucket (= 2N
	// total) rather than the configured N total. We
	// concatenate the two slices, cap once, and partition
	// back by identity so the budget is genuinely shared
	// across the two halves.
	combined := make([]LabelledPair, 0, len(positives)+len(negatives))
	combined = append(combined, positives...)
	combined = append(combined, negatives...)
	positiveIDs := make(map[string]struct{}, len(positives))
	for _, p := range positives {
		positiveIDs[p.EpisodeID] = struct{}{}
	}

	capped := applyActorCap(combined, opts)

	keptPos := make([]LabelledPair, 0, len(positives))
	keptNeg := make([]LabelledPair, 0, len(negatives))
	for _, p := range capped.kept {
		if _, isPositive := positiveIDs[p.EpisodeID]; isPositive {
			keptPos = append(keptPos, p)
		} else {
			keptNeg = append(keptNeg, p)
		}
	}

	return PullResult{
		Positives:    keptPos,
		Negatives:    keptNeg,
		CappedActors: capped.dropped,
	}, nil
}

// scanPositives returns the positive Episode rows per the
// PullPairs doc rules.
//
// Two-phase UNION:
//   - SELECT every kind='synthetic_positive' WITHIN the trailing
//     `windowStart` window (impl-plan §6.4 line 1119: "Pull
//     labelled training pairs ... last 90 days"). A synthetic
//     positive's `created_at` is the moment the consolidator
//     promoted it, which is the supervised-signal age the
//     trainer cares about. The LATERAL lookup attributes the
//     originating `episode_update.actor` so the §9.4 per-actor
//     cap applies to operator-noisy positive corrections too
//     (not only the negative parent).
//   - UNION ALL kind='agent' AND outcome='success' AND
//     created_at >= windowStart. Plain success episodes carry
//     no human-correction attribution so they emit a NULL
//     correction_actor and are exempt from the cap.
//
// The shape of the SELECT list is shared with scanNegatives so
// the row scanner is identical.
func scanPositives(ctx context.Context, db *sql.DB, windowStart time.Time) ([]LabelledPair, error) {
	const q = `
		SELECT sp.episode_id::text,
		       sp.kind::text,
		       sp.outcome::text,
		       sp.created_at,
		       sp.repo_id::text,
		       coalesce(sp.context_id::text, ''),
		       coalesce(sp.action, '{}'::jsonb),
		       coalesce(sp.corrected_action, 'null'::jsonb),
		       eu.actor::text AS correction_actor,
		       eu.created_at  AS correction_update_at
		FROM episode sp
		LEFT JOIN LATERAL (
		    SELECT actor, created_at
		    FROM episode_update
		    WHERE episode_id = sp.synthesized_from_parent_episode_id
		      AND new_outcome = 'human_corrected'
		    ORDER BY created_at DESC
		    LIMIT 1
		) eu ON true
		WHERE sp.kind = 'synthetic_positive'
		  AND sp.created_at >= $1

		UNION ALL

		SELECT episode_id::text,
		       kind::text,
		       outcome::text,
		       created_at,
		       repo_id::text,
		       coalesce(context_id::text, ''),
		       coalesce(action, '{}'::jsonb),
		       coalesce(corrected_action, 'null'::jsonb),
		       NULL::text     AS correction_actor,
		       NULL::timestamptz AS correction_update_at
		FROM episode
		WHERE kind = 'agent'
		  AND outcome = 'success'
		  AND created_at >= $1
		ORDER BY created_at, episode_id
	`
	return scanPairRows(ctx, db, q, windowStart)
}

// scanNegatives returns the negative Episode rows per the
// PullPairs doc rules.
//
// Two-phase UNION:
//   - SELECT kind='agent' AND outcome IN
//     ('failure','degraded') AND created_at >= windowStart.
//   - UNION ALL the PARENT of every synthetic_positive
//     WHOSE CHILD's created_at >= windowStart (the trailing
//     90-day requirement applies to the correction signal,
//     not to the parent's original Episode time). The LATERAL
//     lookup attributes the most-recent `human_corrected`
//     EpisodeUpdate so the §9.4 per-actor cap is applied to
//     the operator that produced the correction.
//
// The LATERAL join is bounded LIMIT 1 so it stays an index
// seek on `episode_update_episode_created_idx` (0008 migration
// `(episode_id, created_at DESC)`).
func scanNegatives(ctx context.Context, db *sql.DB, windowStart time.Time) ([]LabelledPair, error) {
	const q = `
		SELECT episode_id::text,
		       kind::text,
		       outcome::text,
		       created_at,
		       repo_id::text,
		       coalesce(context_id::text, ''),
		       coalesce(action, '{}'::jsonb),
		       coalesce(corrected_action, 'null'::jsonb),
		       NULL::text     AS correction_actor,
		       NULL::timestamptz AS correction_update_at
		FROM episode
		WHERE kind = 'agent'
		  AND outcome IN ('failure', 'degraded')
		  AND created_at >= $1

		UNION ALL

		SELECT parent.episode_id::text,
		       parent.kind::text,
		       parent.outcome::text,
		       parent.created_at,
		       parent.repo_id::text,
		       coalesce(parent.context_id::text, ''),
		       coalesce(parent.action, '{}'::jsonb),
		       coalesce(parent.corrected_action, 'null'::jsonb),
		       eu.actor::text AS correction_actor,
		       eu.created_at  AS correction_update_at
		FROM episode sp
		JOIN episode parent
		  ON parent.episode_id = sp.synthesized_from_parent_episode_id
		LEFT JOIN LATERAL (
		    SELECT actor, created_at
		    FROM episode_update
		    WHERE episode_id = parent.episode_id
		      AND new_outcome = 'human_corrected'
		    ORDER BY created_at DESC
		    LIMIT 1
		) eu ON true
		WHERE sp.kind = 'synthetic_positive'
		  AND sp.created_at >= $1
		ORDER BY created_at, episode_id
	`
	return scanPairRows(ctx, db, q, windowStart)
}

func scanPairRows(ctx context.Context, db *sql.DB, query string, windowStart time.Time) ([]LabelledPair, error) {
	rows, err := db.QueryContext(ctx, query, windowStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]LabelledPair, 0, 64)
	for rows.Next() {
		var (
			p                    LabelledPair
			actor                sql.NullString
			correctionUpdateTime sql.NullTime
			actionRaw            []byte
			correctedRaw         []byte
		)
		if err := rows.Scan(
			&p.EpisodeID, &p.EpisodeKind, &p.Outcome, &p.CreatedAt, &p.RepoID,
			&p.ContextID, &actionRaw, &correctedRaw, &actor, &correctionUpdateTime,
		); err != nil {
			return nil, err
		}
		if len(actionRaw) > 0 {
			p.Action = json.RawMessage(append([]byte(nil), actionRaw...))
		}
		if !isJSONNull(correctedRaw) {
			p.CorrectedAction = json.RawMessage(append([]byte(nil), correctedRaw...))
		}
		if actor.Valid {
			p.CorrectionActor = actor.String
		}
		if correctionUpdateTime.Valid {
			p.CorrectionUpdateAt = correctionUpdateTime.Time.UTC()
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// isJSONNull is true when the raw JSON message is exactly the
// literal `null` (the COALESCE default for the
// corrected_action column when no correction exists). Keeping
// that as the in-Go zero value (nil) is cleaner than a
// json.RawMessage carrying "null" bytes which downstream
// consumers have to special-case.
func isJSONNull(b []byte) bool {
	switch string(b) {
	case "", "null":
		return true
	}
	return false
}

// hydrateContext fills SeedNodeIDs / SeedEdgeIDs /
// SeedConceptIDs / QueryJSON on every pair that has a
// non-empty ContextID. Issues ONE batched IN-list query so a
// 10k-pair training set produces one round-trip, not 10k.
func hydrateContext(ctx context.Context, db *sql.DB, pairs []LabelledPair) error {
	if len(pairs) == 0 {
		return nil
	}
	ctxIDSet := make(map[string]struct{}, len(pairs))
	for _, p := range pairs {
		if p.ContextID != "" {
			ctxIDSet[p.ContextID] = struct{}{}
		}
	}
	if len(ctxIDSet) == 0 {
		return nil
	}
	ids := make([]string, 0, len(ctxIDSet))
	for id := range ctxIDSet {
		ids = append(ids, id)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT context_id::text,
		       query_json,
		       coalesce(node_ids,    ARRAY[]::uuid[])::uuid[]::text[],
		       coalesce(edge_ids,    ARRAY[]::uuid[])::uuid[]::text[],
		       coalesce(concept_ids, ARRAY[]::uuid[])::uuid[]::text[]
		FROM recall_context_log
		WHERE context_id::text = ANY($1)
	`, pq.Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()

	type ctxRow struct {
		query    json.RawMessage
		nodeIDs  []string
		edgeIDs  []string
		conIDs   []string
	}
	byID := make(map[string]ctxRow, len(ids))
	for rows.Next() {
		var (
			id      string
			qraw    []byte
			nodeIDs pq.StringArray
			edgeIDs pq.StringArray
			conIDs  pq.StringArray
		)
		if err := rows.Scan(&id, &qraw, &nodeIDs, &edgeIDs, &conIDs); err != nil {
			return err
		}
		byID[id] = ctxRow{
			query:   json.RawMessage(append([]byte(nil), qraw...)),
			nodeIDs: []string(nodeIDs),
			edgeIDs: []string(edgeIDs),
			conIDs:  []string(conIDs),
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range pairs {
		if pairs[i].ContextID == "" {
			continue
		}
		row, ok := byID[pairs[i].ContextID]
		if !ok {
			continue
		}
		pairs[i].QueryJSON = row.query
		pairs[i].RecallQuery = extractRecallQuery(row.query)
		pairs[i].SeedNodeIDs = row.nodeIDs
		pairs[i].SeedEdgeIDs = row.edgeIDs
		pairs[i].SeedConceptIDs = row.conIDs
	}
	return nil
}

// extractRecallQuery pulls the natural-language `.query`
// string out of a `recall_context_log.query_json` payload
// for downstream cross-encoder training. The agent-api
// recall handler persists query_json as
// `{"query": "...", "kinds": [...], "k": N, ...}` (see
// `internal/agentapi/recall.go:buildContextLogInput`); the
// trainer needs the bare `query` field to build the
// (query, document) cross-encoder pair on the same surface
// the sidecar's `/rank` endpoint receives at recall time.
//
// Returns "" when the payload is absent, malformed, or
// missing the `query` field — every caller falls back to
// the seed-id projection path which preserves train/rank
// surface congruence for legacy rows without a `query`
// field. Closes iter-5 review item 1+2: the trainer now
// carries the EXACT string the recall path posts to
// `/rank`, not an empty default.
func extractRecallQuery(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ""
	}
	return probe.Query
}

// hydrateObservations fills LabelledPair.Observations for
// every pair in one batched query.
func hydrateObservations(ctx context.Context, db *sql.DB, pairs []LabelledPair) error {
	if len(pairs) == 0 {
		return nil
	}
	epIDSet := make(map[string]struct{}, len(pairs))
	for _, p := range pairs {
		epIDSet[p.EpisodeID] = struct{}{}
	}
	ids := make([]string, 0, len(epIDSet))
	for id := range epIDSet {
		ids = append(ids, id)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT episode_id::text,
		       role::text,
		       weight,
		       coalesce(node_id::text,    ''),
		       coalesce(edge_id::text,    ''),
		       coalesce(concept_id::text, '')
		FROM observation
		WHERE episode_id::text = ANY($1)
		ORDER BY episode_id, created_at, observation_id
	`, pq.Array(ids))
	if err != nil {
		return err
	}
	defer rows.Close()

	byEp := make(map[string][]LabelledObservation, len(ids))
	for rows.Next() {
		var (
			epID, role          string
			weight              float64
			nodeID, edgeID, cID string
		)
		if err := rows.Scan(&epID, &role, &weight, &nodeID, &edgeID, &cID); err != nil {
			return err
		}
		byEp[epID] = append(byEp[epID], LabelledObservation{
			Role: role, Weight: weight,
			NodeID: nodeID, EdgeID: edgeID, ConceptID: cID,
		})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for i := range pairs {
		pairs[i].Observations = byEp[pairs[i].EpisodeID]
	}
	return nil
}

// capResult is the output of applyActorCap. Kept as a private
// type because the public PullResult shape already captures
// what callers need.
type capResult struct {
	kept    []LabelledPair
	dropped map[string]uint64
}

// applyActorCap implements the §9.4 per-actor sliding-window
// cap. The cap applies to any pair with a non-empty
// CorrectionActor (both the correction-derived positives --
// synthetic_positive rows whose originating EpisodeUpdate
// attributed an actor -- AND the correction-derived
// negatives -- parent Episodes of those synthetic positives).
//
// Algorithm:
//
//  1. Walk all candidates; pass through every pair whose
//     CorrectionActor is empty OR whose CorrectionUpdateAt is
//     OUTSIDE the cap's sliding window (an old correction
//     does not consume current cap budget).
//  2. For each in-window correction, sort by CorrectionUpdateAt
//     ascending (then by EpisodeID for tie-breaking
//     determinism); keep the first N per actor; drop the rest.
//  3. The dropped map is keyed by actor enum value with the
//     drop count per actor.
//
// Determinism: the sort key (CorrectionUpdateAt ASC, EpisodeID
// ASC) makes the dropped set reproducible across reruns over
// the same input, which is essential for the
// `DeriveVersion` fingerprint to remain stable.
//
// When opts.ActorCapPerWindow <= 0 the cap is disabled and
// every pair passes through unchanged.
func applyActorCap(pairs []LabelledPair, opts PullOpts) capResult {
	dropped := map[string]uint64{}
	if opts.ActorCapPerWindow <= 0 {
		return capResult{kept: append([]LabelledPair(nil), pairs...), dropped: dropped}
	}
	if opts.ActorCapWindow <= 0 {
		opts.ActorCapWindow = time.Hour
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	windowStart := now.Add(-opts.ActorCapWindow)

	type idx struct {
		pos int
		key time.Time
	}
	inWindowByActor := map[string][]idx{}
	kept := make([]LabelledPair, 0, len(pairs))
	keep := make([]bool, len(pairs))

	for i, p := range pairs {
		if p.CorrectionActor == "" || p.CorrectionUpdateAt.IsZero() {
			keep[i] = true
			continue
		}
		if p.CorrectionUpdateAt.Before(windowStart) {
			// Old correction: not subject to the cap.
			keep[i] = true
			continue
		}
		inWindowByActor[p.CorrectionActor] = append(
			inWindowByActor[p.CorrectionActor],
			idx{pos: i, key: p.CorrectionUpdateAt},
		)
	}

	for actor, candidates := range inWindowByActor {
		sort.Slice(candidates, func(a, b int) bool {
			ca, cb := candidates[a], candidates[b]
			if !ca.key.Equal(cb.key) {
				return ca.key.Before(cb.key)
			}
			return pairs[ca.pos].EpisodeID < pairs[cb.pos].EpisodeID
		})
		for n, c := range candidates {
			if n < opts.ActorCapPerWindow {
				keep[c.pos] = true
				continue
			}
			dropped[actor]++
		}
	}

	for i, p := range pairs {
		if keep[i] {
			kept = append(kept, p)
		}
	}
	return capResult{kept: kept, dropped: dropped}
}
