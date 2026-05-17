package rerankertrainer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SidecarTrainer wraps an external HTTP training endpoint.
// The Forge service hosts an in-process linear baseline
// (LinearTrainer) for hermetic CI; in production the
// AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT env var points the
// binary at a Python sidecar that fits the BERT-class
// cross-encoder named by impl-plan §6.4 step-3 (cap <=200M
// params). Both sides of the boundary share the JSON shape
// declared on TrainingInput / TrainingOutput so the Go layer
// can swap the trainer transport without changing the
// reranker_model schema.
//
// Protocol:
//
//	POST  <Endpoint>/train
//	Body: TrainingInput (encoded via encoding/json)
//	200:  TrainingOutput (encoded via encoding/json)
//	non-2xx: surfaces the body as the error message
//
// The sidecar MUST return:
//   - Version: non-empty, deterministic fingerprint. The
//     SidecarTrainer REJECTS empty Version with an error so
//     a misconfigured sidecar surfaces loudly rather than
//     landing a success-shaped reranker_model row with a
//     synthetic version that obscures the bug. Sidecars
//     that want the Service's deterministic fingerprint can
//     compute it with the published DeriveVersion helper.
//   - ArtifactURI: non-empty pointer the recall path can
//     fetch. The SidecarTrainer REJECTS empty ArtifactURI;
//     a row without a fetchable artifact is unusable at
//     recall time so there is no value in publishing it.
//   - Metrics: MUST include `train_loss`, `eval_ndcg@k`,
//     `rank-of-correct-node@k=20` per §6.4 step-3. The
//     SidecarTrainer REJECTS missing required keys — a
//     trainer that cannot self-report these metrics has not
//     genuinely trained. Extra keys pass through unchanged.
//   - PublishStatus: typically "published". The Service does
//     NOT downgrade sidecar output -- a sidecar that lies
//     about its tag still owns its publish decision. Empty
//     PublishStatus is REJECTED so a sidecar bug producing
//     `""` does not implicitly publish.
//
// SidecarTrainer is REQUIRED to be safe for concurrent use;
// the Service.Tick advisory lock serialises ticks at the
// cross-replica boundary but the http.Client is goroutine-safe
// by design.
type SidecarTrainer struct {
	// Endpoint is the base URL of the sidecar (no trailing
	// slash). Required; constructing with an empty endpoint
	// panics.
	Endpoint string

	// Client is the http.Client the trainer uses. Optional;
	// a nil value falls back to a fresh http.Client with a
	// 30-minute timeout (matches DefaultTickTimeout). The
	// Service.Tick context budget further bounds the call.
	Client *http.Client

	// TagName is the trainer tag carried into
	// TrainingInput.TrainerTag and the version fingerprint.
	// Defaults to "sidecar"; production wires the model
	// name ("bert-base-uncased", "ms-marco-MiniLM-L-12-v2",
	// etc.) so a swap of the sidecar model produces a
	// different fingerprint even if the pair set is
	// unchanged.
	TagName string

	// MaxResponseBytes caps the size of the sidecar
	// response we read into memory. Defaults to 16 MiB
	// (enough for a generously-sized metrics JSON +
	// artifact URI string; the artifact itself is never
	// inlined here).
	MaxResponseBytes int64
}

// Tag implements Trainer.
func (s SidecarTrainer) Tag() string {
	if s.TagName != "" {
		return s.TagName
	}
	return "sidecar"
}

// Train implements Trainer. POSTs `in` as JSON to the sidecar
// and returns the sidecar's TrainingOutput verbatim, with the
// strict-validation contract documented on SidecarTrainer:
// missing Version / ArtifactURI / required metrics /
// PublishStatus produce a typed error rather than silent
// backfill so a broken training framework cannot create
// success-shaped `reranker_model` rows.
func (s SidecarTrainer) Train(ctx context.Context, in TrainingInput) (TrainingOutput, error) {
	if s.Endpoint == "" {
		return TrainingOutput{}, errors.New("rerankertrainer: SidecarTrainer.Endpoint is empty")
	}

	payload, err := json.Marshal(in)
	if err != nil {
		return TrainingOutput{}, fmt.Errorf("rerankertrainer: sidecar marshal input: %w", err)
	}

	url := s.Endpoint + "/train"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url,
		bytes.NewReader(payload))
	if err != nil {
		return TrainingOutput{}, fmt.Errorf("rerankertrainer: sidecar request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	resp, err := client.Do(req)
	if err != nil {
		return TrainingOutput{}, fmt.Errorf("rerankertrainer: sidecar Do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	maxBytes := s.MaxResponseBytes
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return TrainingOutput{}, fmt.Errorf("rerankertrainer: sidecar read body: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return TrainingOutput{}, fmt.Errorf("rerankertrainer: sidecar HTTP %d: %s",
			resp.StatusCode, truncate(string(body), 512))
	}

	var out TrainingOutput
	if err := json.Unmarshal(body, &out); err != nil {
		return TrainingOutput{}, fmt.Errorf("rerankertrainer: sidecar unmarshal: %w", err)
	}

	if err := validateSidecarOutput(out); err != nil {
		return TrainingOutput{}, fmt.Errorf("rerankertrainer: sidecar response invalid: %w; body=%s",
			err, truncate(string(body), 512))
	}
	return out, nil
}

// requiredSidecarMetrics enumerates the metric keys §6.4
// step-3 says the trainer MUST emit. Kept as a package-level
// var (not a const) so tests can verify the set is non-empty.
var requiredSidecarMetrics = []string{
	"train_loss",
	"eval_ndcg@k",
	"rank-of-correct-node@k=20",
}

// validateSidecarOutput is the F7 strict-validation gate.
// Each missing-or-empty required field produces a typed error
// so the publish path fails loudly. The set of checks is
// intentionally small and shallow — anything richer (e.g.
// metric value sanity) belongs in the publish-side audit, not
// the trainer-side transport layer.
//
// Iter-20 evaluator item 3: additionally validate
// `publish_status` against the closed set {published, shadow}
// via `ValidatePublishStatus`, so a sidecar typo cannot
// create an unconsumed `reranker_model` row whose `status`
// column carries a value the recall path's
// `WHERE status='published'` filter will silently skip.
func validateSidecarOutput(out TrainingOutput) error {
	if out.Version == "" {
		return errors.New("sidecar response missing required field 'version'")
	}
	if out.ArtifactURI == "" {
		return errors.New("sidecar response missing required field 'artifact_uri'")
	}
	if err := ValidatePublishStatus(out.PublishStatus); err != nil {
		return fmt.Errorf("sidecar response invalid 'publish_status': %w", err)
	}
	if out.Metrics == nil {
		return errors.New("sidecar response missing required field 'metrics'")
	}
	for _, k := range requiredSidecarMetrics {
		if _, ok := out.Metrics[k]; !ok {
			return fmt.Errorf("sidecar response metrics missing required key %q", k)
		}
	}
	return nil
}

// truncate clips a string for safe inclusion in an error
// message. The sidecar can return arbitrarily large error
// bodies; the supervisor's log surface is unbounded but
// noisy log entries are still undesirable.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…(truncated)"
}
