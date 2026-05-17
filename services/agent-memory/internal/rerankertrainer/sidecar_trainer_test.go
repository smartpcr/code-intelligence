package rerankertrainer

// Unit tests for SidecarTrainer's F7 strict-validation
// contract. Each required field has a missing-field test that
// MUST produce a typed error rather than silent backfill.
//
// Why every-field coverage: prior iter (iter-2) wired silent
// defaults for empty Version, empty ArtifactURI, missing
// required metrics, and empty PublishStatus. The evaluator
// flagged this as F7 ("malformed outputs are converted into
// publishable defaults instead of failing"). These tests
// guard against drift back to the silent-default shape.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startFakeSidecar returns an HTTP test server whose /train
// endpoint replies with the marshalled `body` payload. Used
// by the strict-validation tests to inject malformed sidecar
// responses without needing a real Python sidecar.
func startFakeSidecar(t *testing.T, body any) *httptest.Server {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("test setup: marshal sidecar body: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSidecarTrainer_RejectsMissingVersion(t *testing.T) {
	t.Parallel()
	srv := startFakeSidecar(t, map[string]any{
		"version":        "",
		"artifact_uri":   "s3://bucket/model",
		"publish_status": StatusPublished,
		"metrics": map[string]float64{
			"train_loss":                0.5,
			"eval_ndcg@k":               0.7,
			"rank-of-correct-node@k=20": 3,
		},
	})
	_, err := SidecarTrainer{Endpoint: srv.URL}.Train(context.Background(), TrainingInput{})
	if err == nil {
		t.Fatalf("expected error on empty Version, got nil")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Errorf("error %q must reference the missing field 'version'", err.Error())
	}
}

func TestSidecarTrainer_RejectsMissingArtifactURI(t *testing.T) {
	t.Parallel()
	srv := startFakeSidecar(t, map[string]any{
		"version":        "v-from-sidecar",
		"artifact_uri":   "",
		"publish_status": StatusPublished,
		"metrics": map[string]float64{
			"train_loss":                0.5,
			"eval_ndcg@k":               0.7,
			"rank-of-correct-node@k=20": 3,
		},
	})
	_, err := SidecarTrainer{Endpoint: srv.URL}.Train(context.Background(), TrainingInput{})
	if err == nil {
		t.Fatalf("expected error on empty ArtifactURI, got nil")
	}
	if !strings.Contains(err.Error(), "artifact_uri") {
		t.Errorf("error %q must reference the missing field 'artifact_uri'", err.Error())
	}
}

func TestSidecarTrainer_RejectsMissingPublishStatus(t *testing.T) {
	t.Parallel()
	srv := startFakeSidecar(t, map[string]any{
		"version":        "v-from-sidecar",
		"artifact_uri":   "s3://bucket/model",
		"publish_status": "",
		"metrics": map[string]float64{
			"train_loss":                0.5,
			"eval_ndcg@k":               0.7,
			"rank-of-correct-node@k=20": 3,
		},
	})
	_, err := SidecarTrainer{Endpoint: srv.URL}.Train(context.Background(), TrainingInput{})
	if err == nil {
		t.Fatalf("expected error on empty PublishStatus, got nil")
	}
	if !strings.Contains(err.Error(), "publish_status") {
		t.Errorf("error %q must reference the missing field 'publish_status'", err.Error())
	}
}

func TestSidecarTrainer_RejectsMissingRequiredMetric(t *testing.T) {
	t.Parallel()
	// Drop one required metric per case; the validator MUST
	// reject every variant.
	for _, missing := range requiredSidecarMetrics {
		missing := missing
		t.Run("missing="+missing, func(t *testing.T) {
			t.Parallel()
			metrics := map[string]float64{
				"train_loss":                0.5,
				"eval_ndcg@k":               0.7,
				"rank-of-correct-node@k=20": 3,
			}
			delete(metrics, missing)
			srv := startFakeSidecar(t, map[string]any{
				"version":        "v-from-sidecar",
				"artifact_uri":   "s3://bucket/model",
				"publish_status": StatusPublished,
				"metrics":        metrics,
			})
			_, err := SidecarTrainer{Endpoint: srv.URL}.Train(context.Background(), TrainingInput{})
			if err == nil {
				t.Fatalf("expected error on missing metric %q, got nil", missing)
			}
			if !strings.Contains(err.Error(), missing) {
				t.Errorf("error %q must reference the missing metric key %q", err.Error(), missing)
			}
		})
	}
}

func TestSidecarTrainer_RejectsNilMetricsMap(t *testing.T) {
	t.Parallel()
	srv := startFakeSidecar(t, map[string]any{
		"version":        "v-from-sidecar",
		"artifact_uri":   "s3://bucket/model",
		"publish_status": StatusPublished,
		"metrics":        nil,
	})
	_, err := SidecarTrainer{Endpoint: srv.URL}.Train(context.Background(), TrainingInput{})
	if err == nil {
		t.Fatalf("expected error on nil Metrics, got nil")
	}
	if !strings.Contains(err.Error(), "metrics") {
		t.Errorf("error %q must reference the missing 'metrics' field", err.Error())
	}
}

func TestSidecarTrainer_AcceptsCompleteResponse(t *testing.T) {
	t.Parallel()
	// Happy path: every required field present → success.
	// Pins the contract so a future refactor cannot
	// accidentally reject a well-formed sidecar response.
	srv := startFakeSidecar(t, map[string]any{
		"version":        "v-from-sidecar",
		"artifact_uri":   "s3://bucket/model",
		"publish_status": StatusPublished,
		"metrics": map[string]float64{
			"train_loss":                0.5,
			"eval_ndcg@k":               0.7,
			"rank-of-correct-node@k=20": 3,
			"extra_diagnostic":          42,
		},
	})
	out, err := SidecarTrainer{Endpoint: srv.URL}.Train(context.Background(), TrainingInput{})
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	if out.Version != "v-from-sidecar" {
		t.Errorf("Version = %q, want %q", out.Version, "v-from-sidecar")
	}
	if out.ArtifactURI != "s3://bucket/model" {
		t.Errorf("ArtifactURI = %q, want %q", out.ArtifactURI, "s3://bucket/model")
	}
	// Extra metrics pass through unchanged.
	if got := out.Metrics["extra_diagnostic"]; got != 42 {
		t.Errorf("extra_diagnostic = %v, want 42 (extra metrics must pass through)", got)
	}
}

// TestSidecarTrainer_RejectsTypoPublishStatus pins the
// closed-set gate added for iter-20 evaluator item 3. The
// `reranker_model.status` column is plain text (no CHECK
// constraint in migration 0012_run_tables.sql), so a sidecar
// typo like `publsihed` would silently land an unconsumed
// row -- the recall path's `WHERE status='published'` filter
// would skip it forever. ValidatePublishStatus must reject
// any value outside {shadow, published}.
func TestSidecarTrainer_RejectsTypoPublishStatus(t *testing.T) {
	t.Parallel()
	// `retired` is operator-only (see trainer.go closed-set
	// rationale) AND a literal typo must both be rejected.
	for _, bad := range []string{"publsihed", "PUBLISHED", "live", StatusRetired, " published"} {
		bad := bad
		t.Run("bad="+bad, func(t *testing.T) {
			t.Parallel()
			srv := startFakeSidecar(t, map[string]any{
				"version":        "v-from-sidecar",
				"artifact_uri":   "s3://bucket/model",
				"publish_status": bad,
				"metrics": map[string]float64{
					"train_loss":                0.5,
					"eval_ndcg@k":               0.7,
					"rank-of-correct-node@k=20": 3,
				},
			})
			_, err := SidecarTrainer{Endpoint: srv.URL}.Train(context.Background(), TrainingInput{})
			if err == nil {
				t.Fatalf("expected error on invalid publish_status %q, got nil", bad)
			}
			if !strings.Contains(err.Error(), "publish_status") {
				t.Errorf("error %q must reference the field 'publish_status'", err.Error())
			}
			if !strings.Contains(err.Error(), "closed set") {
				t.Errorf("error %q must explain the closed-set constraint so operator sees both sides", err.Error())
			}
		})
	}
}

// TestValidatePublishStatus is a focused unit test for the
// shared validator. Exposes the closed-set rule independent
// of the sidecar HTTP boundary so any future caller (e.g. a
// REST admin endpoint that publishes manually) gets the same
// guarantees.
func TestValidatePublishStatus(t *testing.T) {
	t.Parallel()
	t.Run("accepts_shadow", func(t *testing.T) {
		t.Parallel()
		if err := ValidatePublishStatus(StatusShadow); err != nil {
			t.Errorf("ValidatePublishStatus(StatusShadow) = %v, want nil", err)
		}
	})
	t.Run("accepts_published", func(t *testing.T) {
		t.Parallel()
		if err := ValidatePublishStatus(StatusPublished); err != nil {
			t.Errorf("ValidatePublishStatus(StatusPublished) = %v, want nil", err)
		}
	})
	t.Run("rejects_retired", func(t *testing.T) {
		t.Parallel()
		// `retired` is reserved for operator-driven
		// supersede via the agent_memory_admin role; the
		// trainer must NEVER publish directly into
		// retired (no downstream consumer reads it).
		if err := ValidatePublishStatus(StatusRetired); err == nil {
			t.Errorf("ValidatePublishStatus(StatusRetired) = nil, want non-nil (operator-only)")
		}
	})
	t.Run("rejects_empty", func(t *testing.T) {
		t.Parallel()
		if err := ValidatePublishStatus(""); err == nil {
			t.Errorf("ValidatePublishStatus(\"\") = nil, want non-nil")
		}
	})
	t.Run("rejects_typo", func(t *testing.T) {
		t.Parallel()
		if err := ValidatePublishStatus("publsihed"); err == nil {
			t.Errorf("ValidatePublishStatus(\"publsihed\") = nil, want non-nil")
		}
	})
}

// TestIsValidPublishStatus pins the boolean variant. Same
// closed-set as ValidatePublishStatus but returns bool for
// callers that want a predicate without an error allocation.
func TestIsValidPublishStatus(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		StatusShadow:    true,
		StatusPublished: true,
		StatusRetired:   false,
		"":              false,
		"publsihed":     false,
		"PUBLISHED":     false,
	}
	for in, want := range cases {
		in, want := in, want
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			if got := IsValidPublishStatus(in); got != want {
				t.Errorf("IsValidPublishStatus(%q) = %v, want %v", in, got, want)
			}
		})
	}
}
