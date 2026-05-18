package mgmtapi

// Unit tests for [PrometheusMetricsHandler] -- the GET /metrics
// handler the mgmt-api binary registers so operators can scrape
// `mgmt_ingest_spans_total{status,repo_id}`.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrometheusMetricsHandler_GET_returnsExpositionFormat(t *testing.T) {
	t.Parallel()
	m := NewIngestSpansMetrics()
	m.Inc(IngestSpansStatusAccepted, "11111111-2222-3333-4444-555555555555")
	m.Inc(IngestSpansStatusAccepted, "11111111-2222-3333-4444-555555555555")
	m.Inc(IngestSpansStatusValidationError, "11111111-2222-3333-4444-555555555555")
	m.Inc(IngestSpansStatusBackpressure, "22222222-2222-3333-4444-555555555555")

	h := NewPrometheusMetricsHandler(m)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != MetricsContentType {
		t.Errorf("Content-Type = %q, want %q", got, MetricsContentType)
	}
	body := w.Body.String()
	mustContain(t, body, "# HELP mgmt_ingest_spans_total")
	mustContain(t, body, "# TYPE mgmt_ingest_spans_total counter")
	mustContain(t, body, `mgmt_ingest_spans_total{repo_id="11111111-2222-3333-4444-555555555555",status="accepted"} 2`)
	mustContain(t, body, `mgmt_ingest_spans_total{repo_id="11111111-2222-3333-4444-555555555555",status="validation_error"} 1`)
	mustContain(t, body, `mgmt_ingest_spans_total{repo_id="22222222-2222-3333-4444-555555555555",status="backpressure"} 1`)
}

func TestPrometheusMetricsHandler_outputDeterministic_sortedKeys(t *testing.T) {
	t.Parallel()
	m := NewIngestSpansMetrics()
	// Insert in a deliberately non-sorted order.
	m.Inc(IngestSpansStatusBackpressure, "repo-z")
	m.Inc(IngestSpansStatusAccepted, "repo-a")
	m.Inc(IngestSpansStatusAccepted, "repo-b")
	m.Inc(IngestSpansStatusValidationError, "repo-a")

	h := NewPrometheusMetricsHandler(m)
	var b1, b2 bytes.Buffer
	h.write(&b1)
	h.write(&b2)
	if b1.String() != b2.String() {
		t.Fatalf("output not deterministic:\nfirst:\n%s\nsecond:\n%s", b1.String(), b2.String())
	}

	// Verify status keys appear in sorted order in the output.
	idxAccepted := strings.Index(b1.String(), `status="accepted"`)
	idxBackpressure := strings.Index(b1.String(), `status="backpressure"`)
	idxValidationErr := strings.Index(b1.String(), `status="validation_error"`)
	if !(idxAccepted < idxBackpressure && idxBackpressure < idxValidationErr) {
		t.Errorf("status keys not in sorted order: accepted@%d backpressure@%d validation_error@%d",
			idxAccepted, idxBackpressure, idxValidationErr)
	}
}

func TestPrometheusMetricsHandler_HEAD_returnsEmptyBody(t *testing.T) {
	t.Parallel()
	m := NewIngestSpansMetrics()
	m.Inc(IngestSpansStatusAccepted, "repo-x")
	h := NewPrometheusMetricsHandler(m)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodHead, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != MetricsContentType {
		t.Errorf("Content-Type = %q, want %q", got, MetricsContentType)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body length = %d, want 0 for HEAD", w.Body.Len())
	}
}

func TestPrometheusMetricsHandler_wrongMethod_returns405(t *testing.T) {
	t.Parallel()
	h := NewPrometheusMetricsHandler(NewIngestSpansMetrics())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/metrics", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if w.Header().Get("Allow") == "" {
		t.Errorf("Allow header missing on 405")
	}
}

func TestPrometheusMetricsHandler_nilSpans_emitsMetadataOnly(t *testing.T) {
	t.Parallel()
	h := &PrometheusMetricsHandler{Spans: nil}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	mustContain(t, body, "# HELP mgmt_ingest_spans_total")
	mustContain(t, body, "# TYPE mgmt_ingest_spans_total counter")
	if strings.Contains(body, "mgmt_ingest_spans_total{") {
		t.Errorf("body has counter lines but ledger is nil: %s", body)
	}
}

func TestEscapePromLabelValue_special(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"plain", "plain"},
		{`with"quote`, `with\"quote`},
		{`with\backslash`, `with\\backslash`},
		{"line\nbreak", `line\nbreak`},
		{"all\\\"three\n", `all\\\"three\n`},
	}
	for _, c := range cases {
		if got := escapePromLabelValue(c.in); got != c.want {
			t.Errorf("escapePromLabelValue(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func mustContain(t *testing.T, body, sub string) {
	t.Helper()
	if !strings.Contains(body, sub) {
		t.Errorf("body missing %q. body=\n%s", sub, body)
	}
}
