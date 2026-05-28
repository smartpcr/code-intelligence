package telemetry

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPrometheusHandler_GETReturns200WithCanonicalContentType(t *testing.T) {
	tick := NewAggregatorTickMetrics()
	tick.Observe(50 * time.Millisecond)
	h := PrometheusHandler(tick)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != PrometheusContentType {
		t.Errorf("Content-Type = %q; want %q", got, PrometheusContentType)
	}
	body := rec.Body.String()
	if !strings.Contains(body, MetricNameAggregatorTickDurationSeconds+"_count 1") {
		t.Errorf("body missing tick count line; got:\n%s", body)
	}
}

func TestPrometheusHandler_HEADReturns200NoBody(t *testing.T) {
	tick := NewAggregatorTickMetrics()
	tick.Observe(10 * time.Millisecond)
	h := PrometheusHandler(tick)

	req := httptest.NewRequest(http.MethodHead, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body = %d bytes; want 0", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Type"); got != PrometheusContentType {
		t.Errorf("HEAD Content-Type = %q; want %q", got, PrometheusContentType)
	}
}

func TestPrometheusHandler_POSTReturns405(t *testing.T) {
	h := PrometheusHandler()
	req := httptest.NewRequest(http.MethodPost, "/metrics", strings.NewReader(""))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d; want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Errorf("Allow header = %q; want \"GET, HEAD\"", got)
	}
}

func TestPrometheusHandler_NilCollectorSkipped(t *testing.T) {
	// PrometheusHandler must accept a nil collector
	// without panicking -- composition roots wire
	// metrics conditionally.
	h := PrometheusHandler(nil, NewWALReplayMetrics(), nil)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "# TYPE "+MetricNameWALReplayDurationSeconds+" histogram") {
		t.Errorf("missing WAL replay header; body:\n%s", rec.Body.String())
	}
}

// TestPrometheusHandler_CollectorErrorReturns500 verifies
// the buffer-before-flush + 500-on-error contract. A
// mid-collector failure must NOT leave a half-flushed wire
// with a 200 status.
func TestPrometheusHandler_CollectorErrorReturns500(t *testing.T) {
	bad := CollectorFunc(func(_ io.Writer) (int, error) {
		return 0, errBadCollector
	})
	h := PrometheusHandler(bad)
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d; want 500", rec.Code)
	}
}

var errBadCollector = errors.New("rendering failed")
