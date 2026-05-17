package rerankertrainer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestDeriveVersion_Deterministic asserts that two calls to
// DeriveVersion over byte-identical TrainingInput produce the
// SAME version string. This is the idempotency contract the
// `reranker_model.version` UNIQUE index relies on -- a tick
// that crashes after Train but before the INSERT must be
// safe to retry without producing a duplicate row.
func TestDeriveVersion_Deterministic(t *testing.T) {
	in := TrainingInput{
		TrainerTag:  "noop",
		WindowStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
		Positives: []LabelledPair{
			{EpisodeID: "ep-A"},
			{EpisodeID: "ep-B"},
		},
		Negatives: []LabelledPair{
			{EpisodeID: "ep-X"},
		},
	}
	v1 := DeriveVersion(in)
	v2 := DeriveVersion(in)
	if v1 != v2 {
		t.Fatalf("DeriveVersion non-deterministic: %q vs %q", v1, v2)
	}
	if !strings.HasPrefix(v1, "noop-") {
		t.Fatalf("version should be prefixed with TrainerTag; got %q", v1)
	}
	// 6-byte sha256 prefix -> 12 hex chars after "noop-".
	if len(v1) != len("noop-")+12 {
		t.Fatalf("version length: got %d (%q)", len(v1), v1)
	}
}

// TestDeriveVersion_OrderInvariant asserts that re-ordering
// the same Episodes in the input slice does NOT change the
// derived version. The Service guards against a database
// row-order drift between two runs producing two
// "different" versions for the same labelled set.
func TestDeriveVersion_OrderInvariant(t *testing.T) {
	a := TrainingInput{
		TrainerTag:  "noop",
		WindowStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
		Positives: []LabelledPair{
			{EpisodeID: "ep-A"}, {EpisodeID: "ep-B"}, {EpisodeID: "ep-C"},
		},
		Negatives: []LabelledPair{{EpisodeID: "ep-X"}, {EpisodeID: "ep-Y"}},
	}
	b := TrainingInput{
		TrainerTag:  "noop",
		WindowStart: a.WindowStart,
		WindowEnd:   a.WindowEnd,
		Positives: []LabelledPair{
			{EpisodeID: "ep-C"}, {EpisodeID: "ep-A"}, {EpisodeID: "ep-B"},
		},
		Negatives: []LabelledPair{{EpisodeID: "ep-Y"}, {EpisodeID: "ep-X"}},
	}
	if DeriveVersion(a) != DeriveVersion(b) {
		t.Fatalf("DeriveVersion not order-invariant:\nA=%q\nB=%q",
			DeriveVersion(a), DeriveVersion(b))
	}
}

// TestDeriveVersion_SensitiveToTag asserts swapping the
// TrainerTag changes the version. A trainer-implementation
// swap MUST surface as a new fingerprint even if the labelled
// set is unchanged -- otherwise the recall path would consume
// a model produced by code it cannot reason about.
func TestDeriveVersion_SensitiveToTag(t *testing.T) {
	base := TrainingInput{
		TrainerTag:  "noop",
		WindowStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
		Positives:   []LabelledPair{{EpisodeID: "ep-A"}},
	}
	swapped := base
	swapped.TrainerTag = "bert-base"

	if DeriveVersion(base) == DeriveVersion(swapped) {
		t.Fatalf("DeriveVersion must differ across trainer tags; both = %q",
			DeriveVersion(base))
	}
	if !strings.HasPrefix(DeriveVersion(swapped), "bert-base-") {
		t.Fatalf("tag prefix not honoured: %q", DeriveVersion(swapped))
	}
}

// TestDeriveVersion_SensitiveToWindow asserts that shifting
// the WindowEnd by more than one second changes the version
// (the fingerprint rounds to second granularity so sub-second
// drift does NOT perturb it -- that exact behaviour is what
// keeps a tick that runs across a sub-second boundary
// idempotent).
func TestDeriveVersion_SensitiveToWindow(t *testing.T) {
	base := TrainingInput{
		TrainerTag:  "noop",
		WindowStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC),
		Positives:   []LabelledPair{{EpisodeID: "ep-A"}},
	}
	withinSecond := base
	withinSecond.WindowEnd = base.WindowEnd.Add(500 * time.Millisecond)
	nextSecond := base
	nextSecond.WindowEnd = base.WindowEnd.Add(time.Second)

	if DeriveVersion(base) != DeriveVersion(withinSecond) {
		t.Errorf("sub-second WindowEnd drift should NOT change version; %q vs %q",
			DeriveVersion(base), DeriveVersion(withinSecond))
	}
	if DeriveVersion(base) == DeriveVersion(nextSecond) {
		t.Errorf("one-second WindowEnd shift MUST change version; both = %q",
			DeriveVersion(base))
	}
}

// TestNoopTrainer_PublishesShadow asserts the NoopTrainer
// returns PublishStatus=StatusShadow, which is the key
// safeguard the §9.10 staleness gate relies on: the noop
// trainer must NEVER advance the "last published" baseline,
// or operators on dev/CI clusters would see a permanently
// "fresh" model that masks a real outage.
func TestNoopTrainer_PublishesShadow(t *testing.T) {
	out, err := NoopTrainer{}.Train(context.Background(), TrainingInput{
		TrainerTag:  "noop",
		WindowStart: time.Now().Add(-90 * 24 * time.Hour),
		WindowEnd:   time.Now(),
		Positives:   []LabelledPair{{EpisodeID: "ep-A"}},
	})
	if err != nil {
		t.Fatalf("NoopTrainer.Train: %v", err)
	}
	if out.PublishStatus != StatusShadow {
		t.Errorf("PublishStatus: got %q want %q",
			out.PublishStatus, StatusShadow)
	}
	if !strings.HasPrefix(out.ArtifactURI, "noop:") {
		t.Errorf("ArtifactURI should be prefixed 'noop:' so an operator can identify the row: got %q",
			out.ArtifactURI)
	}
}

// TestNoopTrainer_MetricsCarryPairCounts asserts the
// NoopTrainer's metrics_json carries the labelled-pair counts.
// The dev/CI dashboard reads positives/negatives from this
// field to validate the pair-pull SQL did pull data.
func TestNoopTrainer_MetricsCarryPairCounts(t *testing.T) {
	in := TrainingInput{
		TrainerTag:  "noop",
		WindowStart: time.Now().Add(-90 * 24 * time.Hour),
		WindowEnd:   time.Now(),
		Positives: []LabelledPair{
			{EpisodeID: "p1"}, {EpisodeID: "p2"}, {EpisodeID: "p3"},
		},
		Negatives: []LabelledPair{{EpisodeID: "n1"}, {EpisodeID: "n2"}},
	}
	out, err := NoopTrainer{}.Train(context.Background(), in)
	if err != nil {
		t.Fatalf("NoopTrainer.Train: %v", err)
	}
	if got := out.Metrics["positives"]; got != 3 {
		t.Errorf("metrics.positives: got %v want 3", got)
	}
	if got := out.Metrics["negatives"]; got != 2 {
		t.Errorf("metrics.negatives: got %v want 2", got)
	}
	// The contract-listed metric keys must all exist (even at
	// zero) so the JSON blob has a stable shape downstream
	// consumers can parse.
	for _, k := range []string{"train_loss", "eval_ndcg@k", "rank-of-correct-node@k=20"} {
		if _, ok := out.Metrics[k]; !ok {
			t.Errorf("metrics missing required key %q (out=%+v)", k, out.Metrics)
		}
	}
	// Round-trip through json so the test exercises the same
	// path the Service uses when persisting metrics_json.
	if _, err := json.Marshal(out.Metrics); err != nil {
		t.Fatalf("metrics json.Marshal: %v", err)
	}
}

// TestNoopTrainer_Tag asserts the trainer's tag is "noop";
// the binary logs this string at startup and the Service
// uses it as the publish-status downgrade guard.
func TestNoopTrainer_Tag(t *testing.T) {
	tr := NoopTrainer{}
	if got := tr.Tag(); got != "noop" {
		t.Fatalf("NoopTrainer.Tag = %q want \"noop\"", got)
	}
}

// TestDeriveVersion_SensitiveToObservationRoleChange closes
// iter-5 review item 6: the prior fingerprint hashed only
// trainer-tag + window + sorted episode IDs, but the
// LinearTrainer's weights depend on observation roles
// (`concept_hit`/`node_hit`/`edge_hit`). Two ticks with the
// same episode IDs but DIFFERENT observation roles would
// have produced different trained artifacts under the SAME
// `reranker_model.version` — colliding the UNIQUE-version
// idempotency contract. This regression asserts the
// fingerprint now flips when observation roles change.
func TestDeriveVersion_SensitiveToObservationRoleChange(t *testing.T) {
	base := TrainingInput{
		TrainerTag:  "linear",
		WindowStart: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC),
		Positives: []LabelledPair{
			{
				EpisodeID:    "ep-A",
				EpisodeKind:  "agent",
				Observations: []LabelledObservation{{Role: "concept_hit", Weight: 0.5}},
			},
		},
	}
	mutated := TrainingInput{
		TrainerTag:  base.TrainerTag,
		WindowStart: base.WindowStart,
		WindowEnd:   base.WindowEnd,
		Positives: []LabelledPair{
			{
				EpisodeID:    "ep-A",
				EpisodeKind:  "agent",
				Observations: []LabelledObservation{{Role: "node_hit", Weight: 0.5}},
			},
		},
	}
	if DeriveVersion(base) == DeriveVersion(mutated) {
		t.Fatalf("DeriveVersion collided on different observation role — iter-5 item 6 not addressed")
	}
}

// TestDeriveVersion_SensitiveToObservationWeightChange is
// the same iter-5 item 6 regression for the WEIGHT field
// (the LinearTrainer's `featureScoreSignal` is the mean of
// per-observation weights — different weights produce
// different artifacts).
func TestDeriveVersion_SensitiveToObservationWeightChange(t *testing.T) {
	base := TrainingInput{
		TrainerTag: "linear",
		Positives: []LabelledPair{
			{
				EpisodeID:    "ep-A",
				Observations: []LabelledObservation{{Role: "concept_hit", Weight: 0.5}},
			},
		},
	}
	mutated := TrainingInput{
		TrainerTag: "linear",
		Positives: []LabelledPair{
			{
				EpisodeID:    "ep-A",
				Observations: []LabelledObservation{{Role: "concept_hit", Weight: 0.9}},
			},
		},
	}
	if DeriveVersion(base) == DeriveVersion(mutated) {
		t.Fatalf("DeriveVersion collided on different observation weight — iter-5 item 6 not addressed")
	}
}

// TestDeriveVersion_SensitiveToRecallQuery closes iter-5
// review item 3 on the Go side: the sidecar's
// `_derive_version` now hashes `pair.recall_query`, and the
// Go-side trainer fingerprint MUST do the same so two ticks
// with identical episode shapes but different recall
// queries can never collide under the same
// `reranker_model.version`.
func TestDeriveVersion_SensitiveToRecallQuery(t *testing.T) {
	base := TrainingInput{
		TrainerTag: "linear",
		Positives: []LabelledPair{
			{EpisodeID: "ep-A", RecallQuery: "how does auth refresh handle 401?"},
		},
	}
	mutated := TrainingInput{
		TrainerTag: "linear",
		Positives: []LabelledPair{
			{EpisodeID: "ep-A", RecallQuery: "where is the cache invalidator"},
		},
	}
	if DeriveVersion(base) == DeriveVersion(mutated) {
		t.Fatalf("DeriveVersion collided on different recall_query — iter-5 item 3 not addressed")
	}
}

// TestDeriveVersion_SensitiveToSeedNodeIDs closes the gap
// where the prior fingerprint hashed only episode IDs but
// the LinearTrainer and the BERT sidecar both consume seed
// node IDs as the recall surface — two ticks with the same
// episode IDs but different seed sets would have produced
// different trained artifacts under the same version.
func TestDeriveVersion_SensitiveToSeedNodeIDs(t *testing.T) {
	base := TrainingInput{
		TrainerTag: "linear",
		Positives: []LabelledPair{
			{EpisodeID: "ep-A", SeedNodeIDs: []string{"n1", "n2"}},
		},
	}
	mutated := TrainingInput{
		TrainerTag: "linear",
		Positives: []LabelledPair{
			{EpisodeID: "ep-A", SeedNodeIDs: []string{"n3"}},
		},
	}
	if DeriveVersion(base) == DeriveVersion(mutated) {
		t.Fatalf("DeriveVersion collided on different seed_node_ids — iter-5 item 6 not addressed")
	}
}

// TestExtractRecallQuery_ParsesQueryField closes iter-5
// review item 1+2: the trainer's pull layer now extracts
// the natural-language `query` field from
// `recall_context_log.query_json` and surfaces it on
// LabelledPair.RecallQuery so the sidecar receives the
// EXACT string the recall path posts to `/rank`.
func TestExtractRecallQuery_ParsesQueryField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "agent-api shape",
			in:   `{"query":"how does auth refresh","kinds":["method"],"k":5}`,
			want: "how does auth refresh",
		},
		{
			name: "empty payload",
			in:   ``,
			want: "",
		},
		{
			name: "missing query field",
			in:   `{"k":5,"kinds":["concept"]}`,
			want: "",
		},
		{
			name: "malformed json",
			in:   `{not json`,
			want: "",
		},
		{
			name: "empty query string",
			in:   `{"query":""}`,
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRecallQuery(json.RawMessage(tc.in))
			if got != tc.want {
				t.Fatalf("extractRecallQuery(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
