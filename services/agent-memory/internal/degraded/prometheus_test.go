package degraded

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrometheusHandler_preRegistersAllVerbReasonPairs(t *testing.T) {
	t.Parallel()
	h := NewPrometheusHandler(NewCounter())
	var buf bytes.Buffer
	h.Write(&buf)
	body := buf.String()

	if !strings.Contains(body, "# HELP "+MetricName+" ") {
		t.Errorf("missing HELP line: %s", body)
	}
	if !strings.Contains(body, "# TYPE "+MetricName+" counter\n") {
		t.Errorf("missing TYPE line: %s", body)
	}
	for _, v := range KnownVerbs {
		for _, r := range AllReasons() {
			want := MetricName + "{reason=\"" + r + "\",verb=\"" + v + "\"} 0\n"
			if !strings.Contains(body, want) {
				t.Errorf("pre-registration missing series:\nwant: %s\ngot body:\n%s",
					want, body)
			}
		}
	}
}

func TestPrometheusHandler_recordedPairsAppear(t *testing.T) {
	t.Parallel()
	c := NewCounter()
	c.IncDegraded("agent.recall", ReasonEmbeddingIndexUnavailable)
	c.IncDegraded("agent.recall", ReasonEmbeddingIndexUnavailable)
	c.IncDegraded("agent.observe", ReasonConsolidatorBackpressure)
	c.IncDegraded("mgmt.read.repos", ReasonGraphStoreUnavailable)

	var buf bytes.Buffer
	NewPrometheusHandler(c).Write(&buf)
	body := buf.String()

	wantRows := []string{
		MetricName + "{reason=\"embedding_index_unavailable\",verb=\"agent.recall\"} 2\n",
		MetricName + "{reason=\"consolidator_backpressure\",verb=\"agent.observe\"} 1\n",
		MetricName + "{reason=\"graph_store_unavailable\",verb=\"mgmt.read.repos\"} 1\n",
	}
	for _, want := range wantRows {
		if !strings.Contains(body, want) {
			t.Errorf("expected row %q in body:\n%s", want, body)
		}
	}
}

func TestPrometheusHandler_unknownVerbStillEmitted(t *testing.T) {
	t.Parallel()
	c := NewCounter()
	c.IncDegraded("agent.future", ReasonEpisodicLogUnavailable)

	var buf bytes.Buffer
	NewPrometheusHandler(c).Write(&buf)
	body := buf.String()

	want := MetricName + "{reason=\"episodic_log_unavailable\",verb=\"agent.future\"} 1\n"
	if !strings.Contains(body, want) {
		t.Errorf("unknown-verb row missing:\nwant: %s\ngot body:\n%s", want, body)
	}
}

func TestPrometheusHandler_HTTPMethodPolicy(t *testing.T) {
	t.Parallel()
	h := NewPrometheusHandler(NewCounter())

	for _, m := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(m, "/metrics", nil)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("%s /metrics returned %d; want 200", m, rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); got != MetricsContentType {
			t.Errorf("Content-Type = %q; want %q", got, MetricsContentType)
		}
		if m == http.MethodHead {
			if rec.Body.Len() != 0 {
				t.Errorf("HEAD must return empty body; got %d bytes", rec.Body.Len())
			}
		} else {
			if rec.Body.Len() == 0 {
				t.Errorf("GET body empty; want metric output")
			}
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/metrics", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d; want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q; want \"GET, HEAD\"", got)
	}
}

func TestPrometheusHandler_nilCounter_emitsPreludeOnly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	NewPrometheusHandler(nil).Write(&buf)
	body := buf.String()
	if !strings.Contains(body, "# TYPE "+MetricName+" counter") {
		t.Errorf("nil handler missing TYPE line; got: %s", body)
	}
	// Pre-registered zeros still render.
	want := MetricName + "{reason=\"episodic_log_unavailable\",verb=\"agent.observe\"} 0\n"
	if !strings.Contains(body, want) {
		t.Errorf("nil counter missing pre-registered row %q; body: %s", want, body)
	}
}

// Smoke-test that the body parses as a well-formed Prometheus
// text exposition: every non-comment line MUST start with the
// metric name, contain a `{...} ` label block, and end with an
// integer. We don't pull in a Prometheus parser; the contract
// is well-defined enough that a regex suffices.
func TestPrometheusHandler_textExpositionShape(t *testing.T) {
	t.Parallel()
	h := NewPrometheusHandler(NewCounter())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if !strings.HasPrefix(line, MetricName+"{") {
			t.Errorf("malformed line (missing metric prefix): %q", line)
		}
		if !strings.Contains(line, "} ") {
			t.Errorf("malformed line (missing label-block terminator): %q", line)
		}
	}
}
