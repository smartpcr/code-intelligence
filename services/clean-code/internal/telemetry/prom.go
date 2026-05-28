package telemetry

import (
	"bytes"
	"net/http"
)

// PrometheusContentType is the canonical content-type the
// Prometheus text exposition format v0.0.4 advertises. The
// scraper relies on this MIME so the response is parsed as
// metrics rather than treated as opaque text/plain.
const PrometheusContentType = `text/plain; version=0.0.4; charset=utf-8`

// PrometheusHandler returns an [http.Handler] that emits
// the concatenated Prometheus text-exposition output of
// every supplied [Collector], in order. The handler is
// safe to mount at `/metrics` directly:
//
//	mux.Handle("/metrics", telemetry.PrometheusHandler(
//	    sweepMetrics,
//	    aggTickMetrics,
//	    walReplayMetrics,
//	    ruleEngineMetrics,
//	))
//
// A nil collector in the variadic list is skipped (NOT a
// panic) so a composition root that wires metrics in
// optionally does not need branchy nil guards.
//
// The handler buffers the rendered output before writing
// to the response so a partial collector failure (an
// io.Writer error mid-text) does NOT leave the wire
// half-flushed. On a buffer error the handler returns 500.
func PrometheusHandler(collectors ...Collector) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var buf bytes.Buffer
		for _, c := range collectors {
			if c == nil {
				continue
			}
			if _, err := c.WriteText(&buf); err != nil {
				http.Error(w, "metrics render failed: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		w.Header().Set("Content-Type", PrometheusContentType)
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(buf.Bytes())
	})
}
