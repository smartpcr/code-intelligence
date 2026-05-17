package rerankertrainer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"sort"
	"time"
)

// TrainingInput is the data the trainer consumes for one
// fitting pass. The struct is JSON-serialisable so an external
// trainer can be invoked via a subprocess / HTTP boundary
// without rebuilding the binary.
type TrainingInput struct {
	// Positives is the labelled positive set: agent Episodes
	// with `outcome='success'`, all synthetic_positive
	// Episodes (corrected-then-promoted, G7), and any
	// synthetic positive's parent that wasn't already
	// negative.
	Positives []LabelledPair `json:"positives"`

	// Negatives is the labelled negative set: agent Episodes
	// with `outcome IN ('failure','degraded')` plus the
	// pre-correction `human_corrected` parent Episodes (whose
	// synthetic_positive child is the operator's intended
	// answer).
	Negatives []LabelledPair `json:"negatives"`

	// WindowStart is the lower bound of the trailing
	// Episode-time window the trainer pulled pairs from.
	// Per impl-plan §6.4 line 1119 ("last 90 days") this
	// window applies UNIFORMLY across positive and negative
	// rows, including synthetic_positive Episodes and their
	// parents -- a stale correction signal is not retrained
	// against indefinitely.
	WindowStart time.Time `json:"window_start"`

	// WindowEnd is the upper bound (typically the Tick's wall
	// clock at the start of pair pulling).
	WindowEnd time.Time `json:"window_end"`

	// TrainerTag identifies the training framework the
	// trainer expects to run (e.g. "noop", "bert-base"). Part
	// of the version fingerprint so a swap of the trainer
	// implementation produces a different `version` even if
	// the input set is unchanged.
	TrainerTag string `json:"trainer_tag"`
}

// TrainingOutput is what the trainer returns. The Service
// writes Version + ArtifactURI + Metrics onto the
// `reranker_model` row.
type TrainingOutput struct {
	// Version is the deterministic version string the
	// trainer produced. MUST be derivable from the
	// TrainingInput so a retry over identical input yields
	// the same string. The Service's INSERT relies on
	// `reranker_model.version`'s UNIQUE index to make
	// duplicate publishes a no-op.
	Version string `json:"version"`

	// ArtifactURI is an opaque pointer to the trained model
	// blob. The Service does not interpret it -- it is
	// written verbatim onto `reranker_model.artifact_uri`.
	// For the in-process NoopTrainer this is the literal
	// "noop:" prefix plus the version so an operator
	// inspecting the row can tell the model was a no-op.
	ArtifactURI string `json:"artifact_uri"`

	// Metrics carries the §6.4 step-3 contract metrics
	// (`train_loss`, `eval_ndcg@k`, `rank-of-correct-node@k=20`)
	// plus any framework-specific extras. Written verbatim
	// to `reranker_model.metrics_json`.
	Metrics map[string]float64 `json:"metrics"`

	// PublishStatus is the `reranker_model.status` value the
	// Service writes for this output. The NoopTrainer
	// returns `'shadow'` so the §9.10 stale flag keeps
	// firing on the last *real* published model; production
	// trainers return `'published'`.
	PublishStatus string `json:"publish_status"`
}

// Trainer is the pluggable model-fitting boundary. v1 ships
// three concrete implementations: the SidecarTrainer (the
// production default when AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT
// is set -- the BERT cross-encoder of record per §6.4
// step-3), the LinearTrainer (in-process logistic baseline
// for CI / thin corpora), and the NoopTrainer (deterministic
// fingerprint only, hermetic-CI opt-in). The binary's
// `loadConfig` REFUSES to start when no trainer is wired
// (iter-3 review item 3 closed the prior "noop default"
// shape).
type Trainer interface {
	// Train produces a TrainingOutput for the input. MUST be
	// deterministic on Version: same input -> same version
	// (this is the idempotency contract the
	// `reranker_model.version` UNIQUE index relies on).
	Train(ctx context.Context, in TrainingInput) (TrainingOutput, error)

	// Tag is the short string the trainer carries into the
	// version fingerprint (see TrainingInput.TrainerTag).
	// Constant for the lifetime of the Trainer instance.
	Tag() string
}

// Closed-set values for `reranker_model.status`. The column
// is text in 0012_run_tables.sql so the application enforces
// the closed set.
const (
	// StatusShadow is the "trained but not yet promoted to
	// recall" tier. The §9.10 stale-flag gate on the recall
	// path filters by `status='published'`, so shadow rows
	// do NOT mask staleness on the last real published row
	// even if a shadow row is fresher.
	StatusShadow = "shadow"

	// StatusPublished is the tier the recall path consumes.
	// Production Trainer implementations write this status;
	// the NoopTrainer never does.
	StatusPublished = "published"

	// StatusRetired marks a previously published row that
	// has been superseded. Not written by the trainer
	// itself -- reserved for operator-driven retirement
	// (manual UPDATE through `agent_memory_admin`).
	StatusRetired = "retired"
)

// validPublishStatuses is the closed set of values the trainer
// pipeline is permitted to insert into the
// `reranker_model.status` text column. `StatusRetired` is
// EXCLUDED on purpose -- it is operator-only (issued by hand
// through the `agent_memory_admin` role on supersede), so a
// trainer publishing a row directly into `retired` would
// silently skip the §9.10 staleness gate AND publish a row
// the recall path will never consume. Iter-20 evaluator
// item 3: without this gate, a sidecar typo
// (`publish_status: "publsihed"`) would land an unconsumed
// `reranker_model` row whose `status` column is whatever the
// sidecar emitted -- the text column itself has no CHECK
// constraint, so the application MUST enforce the closed
// set.
var validPublishStatuses = map[string]struct{}{
	StatusShadow:    {},
	StatusPublished: {},
}

// IsValidPublishStatus reports whether s is one of the values
// the trainer pipeline is permitted to write. See
// `validPublishStatuses` for the canonical set and the
// rationale for excluding StatusRetired.
func IsValidPublishStatus(s string) bool {
	_, ok := validPublishStatuses[s]
	return ok
}

// ValidatePublishStatus is the typed-error variant used by
// the publish-side validation gates. The error message names
// the rejected value AND the accepted set so the operator's
// next iter sees both sides of the constraint.
func ValidatePublishStatus(s string) error {
	if s == "" {
		return errors.New("publish_status is empty (expected 'published' or 'shadow')")
	}
	if !IsValidPublishStatus(s) {
		return fmt.Errorf("publish_status %q is not in the closed set {%q,%q} (StatusRetired is operator-only)",
			s, StatusShadow, StatusPublished)
	}
	return nil
}

// NoopTrainer is an in-process Trainer for hermetic CI. It
// does NO model fitting and returns a deterministic version
// derived from the TrainingInput fingerprint so the lifecycle
// gates (advisory lock, UNIQUE-version idempotency, metric
// publication) all exercise on production-shaped data without
// requiring a Python sidecar in CI. It is opt-in via
// AGENT_MEMORY_RERANKER_TRAINER_KIND=noop. The binary REFUSES
// to start with no trainer configured -- there is no default
// trainer (iter-3 review item 3).
//
// The NoopTrainer publishes its rows as `StatusShadow` so the
// §9.10 staleness contract continues to fire on the last
// REAL published model rather than being silenced by an
// in-process no-op.
type NoopTrainer struct{}

// Train implements Trainer.
func (NoopTrainer) Train(_ context.Context, in TrainingInput) (TrainingOutput, error) {
	version := DeriveVersion(in)
	return TrainingOutput{
		Version:     version,
		ArtifactURI: "noop:" + version,
		Metrics: map[string]float64{
			"train_loss":                0,
			"eval_ndcg@k":               0,
			"rank-of-correct-node@k=20": 0,
			"positives":                 float64(len(in.Positives)),
			"negatives":                 float64(len(in.Negatives)),
		},
		PublishStatus: StatusShadow,
	}, nil
}

// Tag implements Trainer.
func (NoopTrainer) Tag() string { return "noop" }

// DeriveVersion produces a deterministic, idempotent version
// string for a TrainingInput. Two ticks that pull the same
// pair set produce the same version, which the
// `reranker_model.version` UNIQUE index then converts into a
// no-op INSERT. This is the restart-safety story: a Tick that
// crashes after Train but before the INSERT can retry with
// the same input and the second attempt's INSERT will be
// rejected silently rather than producing a duplicate row.
//
// Fingerprint inputs (in canonical order):
//   - trainer tag (TrainingInput.TrainerTag)
//   - window bounds (unix seconds, rounded to second
//     granularity so a sub-second drift on the WindowEnd does
//     not perturb the fingerprint)
//   - per pair, sorted by EpisodeID: episode_id, episode_kind,
//     recall_query (top-level `query` extracted from
//     `recall_context_log.query_json`), sorted seed_node_ids
//     / seed_edge_ids / seed_concept_ids, per-observation
//     {role, weight} pairs (weight rounded to 6 decimals to
//     absorb upstream JSON float jitter), and correction_actor.
//
// Iter-5 review item 6: the prior fingerprint hashed only
// trainer-tag + window-bounds + sorted episode IDs, but the
// LinearTrainer's weights depend on observation roles and
// weights AND the cross-encoder sidecar's weights depend on
// the recall_query string. Two ticks with the same episode IDs
// but different observation labels OR different recall queries
// would have produced different trained artifacts under the
// same `reranker_model.version` — colliding the UNIQUE-version
// idempotency contract. The expanded shape hashes the SAME
// LOGICAL pair fields as the Python sidecar's
// `_derive_version` (cmd/reranker-sidecar/main.py), but the
// two hashes are NOT byte-comparable by design:
//
//   - This Go-side DeriveVersion drives idempotency at the
//     PostgreSQL `reranker_model.version` UNIQUE-INSERT layer.
//     It prefixes the trainer-kind tag (`<tag>-<sha256[:12]>`)
//     so a linear-trainer run and a sidecar-trainer run on
//     the same window cannot collide under the same row.
//
//   - The sidecar's `_derive_version` drives idempotency at
//     the per-process on-disk artifact short-circuit layer.
//     It hashes `v3|<model_name>|epochs=<N>|<per-pair…>` and
//     returns a bare 16-hex prefix (no trainer-kind tag,
//     because the sidecar only ever produces BERT artifacts).
//
// The two functions cover the same per-pair fields
// (episode_id, episode_kind, recall_query, sorted seeds,
// per-observation role+weight, correction_actor) so a Go-side
// caller can pre-check pair-content collisions before posting,
// but the byte-level outputs differ because the surrounding
// schema differs.
//
// The output format is "<tag>-<sha256[:12]>" so a human
// looking at `reranker_model.version` can immediately tell
// which trainer class produced the row.
func DeriveVersion(in TrainingInput) string {
	h := sha256.New()
	tag := in.TrainerTag
	if tag == "" {
		tag = "noop"
	}
	_, _ = fmt.Fprintf(h, "tag=%s|", tag)
	_, _ = fmt.Fprintf(h, "ws=%d|we=%d|",
		in.WindowStart.UTC().Unix(), in.WindowEnd.UTC().Unix())

	// Hash positives and negatives separately so a label
	// flip between two otherwise-identical sets changes the
	// fingerprint. Within each side, sort by EpisodeID so
	// concurrent ingestion ordering doesn't perturb the
	// hash.
	writePairs := func(label string, pairs []LabelledPair) {
		sorted := make([]LabelledPair, len(pairs))
		copy(sorted, pairs)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].EpisodeID < sorted[j].EpisodeID })
		for _, p := range sorted {
			_, _ = fmt.Fprintf(h, "%s:%s|kind=%s|q=%s|",
				label, p.EpisodeID, p.EpisodeKind, p.RecallQuery)
			writeSortedIDs(h, "nodes", p.SeedNodeIDs)
			writeSortedIDs(h, "edges", p.SeedEdgeIDs)
			writeSortedIDs(h, "concepts", p.SeedConceptIDs)
			h.Write([]byte("|obs="))
			// Observations are NOT sorted: the trainer's
			// LinearTrainer.extractFeatures iterates in
			// insertion order and the first-match-wins kind
			// indicator is order-sensitive. Hashing the
			// presented order preserves the contract.
			for _, o := range p.Observations {
				_, _ = fmt.Fprintf(h, "%s:%.6f,", o.Role, o.Weight)
			}
			_, _ = fmt.Fprintf(h, "|actor=%s\x00", p.CorrectionActor)
		}
	}
	writePairs("+", in.Positives)
	writePairs("-", in.Negatives)

	sum := h.Sum(nil)
	return fmt.Sprintf("%s-%s", tag, hex.EncodeToString(sum[:6]))
}

// writeSortedIDs hashes a `<label>=<id1>,<id2>,...|` segment
// with the IDs sorted so the seed list is order-invariant.
func writeSortedIDs(h hash.Hash, label string, ids []string) {
	_, _ = fmt.Fprintf(h, "|%s=", label)
	sorted := make([]string, len(ids))
	copy(sorted, ids)
	sort.Strings(sorted)
	for _, id := range sorted {
		h.Write([]byte(id))
		h.Write([]byte{','})
	}
}

// sortedEpisodeIDs is retained for callers that need a
// plain ID-only fingerprint (e.g. external tooling). The
// DeriveVersion fingerprint itself no longer uses it — see
// the per-pair hashing loop above.
func sortedEpisodeIDs(pairs []LabelledPair) []string {
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p.EpisodeID)
	}
	sort.Strings(out)
	return out
}

func joinIDs(ids []string) string {
	if len(ids) == 0 {
		return ""
	}
	// Use a separator that cannot appear inside a uuid
	// surface form so two adjacent ids cannot collide with a
	// single id whose hex happens to begin with our separator.
	const sep = "\x1f" // ASCII unit separator
	out := ids[0]
	for _, id := range ids[1:] {
		out += sep + id
	}
	return out
}

// MetricsJSON renders the TrainingOutput.Metrics map as JSON
// suitable for direct writing to `reranker_model.metrics_json`.
// Go's encoding/json sorts map keys at encode time so the
// output is already byte-stable across retries.
func MetricsJSON(m map[string]float64) ([]byte, error) {
	if len(m) == 0 {
		return []byte("{}"), nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("rerankertrainer: marshal metrics: %w", err)
	}
	return out, nil
}
