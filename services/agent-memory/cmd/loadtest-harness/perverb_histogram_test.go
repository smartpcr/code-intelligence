package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// TestVerbLabelledHistogram_EmitsLabelledSeriesPerVerb pins the
// F4 fix: every emitted bucket / sum / count line carries the
// `verb="..."` label, both verbs appear with their own
// cumulative bucket counts, and the HELP / TYPE preamble is
// emitted exactly once. This is the regression guard the
// evaluator asked for ("the /metrics surface cannot reproduce
// per-verb percentiles as documented").
func TestVerbLabelledHistogram_EmitsLabelledSeriesPerVerb(t *testing.T) {
	const metricName = "loadtest_harness_request_duration_seconds"
	h := newVerbLabelledHistogram(metricName, "Per-request latency observed by the agent-memory loadtest harness.",
		[]float64{0.1, 0.5, 1.0})

	// 0.05 lands in le=0.1 (and every higher cumulative bucket).
	// 0.3 lands in le=0.5 (and every higher cumulative bucket).
	h.Observe(reliability.VerbAgentRecall, 0.05)
	h.Observe(reliability.VerbAgentRecall, 0.3)
	h.Observe(reliability.VerbMgmtIngestSpans, 0.05)

	var buf bytes.Buffer
	h.Write(&buf)
	out := buf.String()

	// Exactly one HELP / TYPE preamble for the metric family.
	if got := strings.Count(out, "# HELP "+metricName+" "); got != 1 {
		t.Errorf("# HELP appeared %d times, want exactly 1\n--- output ---\n%s", got, out)
	}
	if got := strings.Count(out, "# TYPE "+metricName+" histogram"); got != 1 {
		t.Errorf("# TYPE appeared %d times, want exactly 1\n--- output ---\n%s", got, out)
	}

	// Cumulative bucket counts per verb. agent.recall sees both
	// samples, so le="0.1" = 1 and le="0.5" = 2 and le="+Inf" = 2.
	wantLines := []string{
		`loadtest_harness_request_duration_seconds_bucket{verb="agent.recall",le="0.1"} 1`,
		`loadtest_harness_request_duration_seconds_bucket{verb="agent.recall",le="0.5"} 2`,
		`loadtest_harness_request_duration_seconds_bucket{verb="agent.recall",le="1"} 2`,
		`loadtest_harness_request_duration_seconds_bucket{verb="agent.recall",le="+Inf"} 2`,
		`loadtest_harness_request_duration_seconds_count{verb="agent.recall"} 2`,
		`loadtest_harness_request_duration_seconds_bucket{verb="mgmt.ingest_spans",le="0.1"} 1`,
		`loadtest_harness_request_duration_seconds_bucket{verb="mgmt.ingest_spans",le="+Inf"} 1`,
		`loadtest_harness_request_duration_seconds_count{verb="mgmt.ingest_spans"} 1`,
		`loadtest_harness_request_duration_seconds_sum{verb="mgmt.ingest_spans"} 0.05`,
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want) {
			t.Errorf("output missing line %q\n--- output ---\n%s", want, out)
		}
	}

	// The pre-fix bug emitted bucket lines WITHOUT a verb label
	// (e.g. `..._bucket{le="0.1"} N`); the fix MUST never emit
	// that shape because Prometheus cannot split it by verb.
	if strings.Contains(out, metricName+`_bucket{le="`) {
		t.Errorf("found unlabelled bucket series (no verb=) — every bucket MUST carry the verb label:\n%s", out)
	}

	// Verbs are emitted in deterministic (sorted) order so scrape
	// diffs and tests stay stable run-over-run.
	agentIdx := strings.Index(out, `verb="agent.recall"`)
	mgmtIdx := strings.Index(out, `verb="mgmt.ingest_spans"`)
	if agentIdx == -1 || mgmtIdx == -1 {
		t.Fatalf("expected both verbs to appear; agentIdx=%d mgmtIdx=%d\n%s", agentIdx, mgmtIdx, out)
	}
	if agentIdx > mgmtIdx {
		t.Errorf("expected agent.recall series before mgmt.ingest_spans (alphabetical); got agent at %d, mgmt at %d", agentIdx, mgmtIdx)
	}
}

// TestVerbLabelledHistogram_ConcurrentObserveDoesNotLoseSamples
// exercises the LoadOrStore race that would otherwise cause two
// goroutines racing for the same NEW verb to each store their
// own histogram (and the loser's sample lands in an orphaned
// instance never seen by Write). N concurrent goroutines each
// emit M samples for the same brand-new verb; the final count
// MUST equal N*M.
func TestVerbLabelledHistogram_ConcurrentObserveDoesNotLoseSamples(t *testing.T) {
	const (
		metricName = "loadtest_harness_request_duration_seconds"
		verb       = "agent.recall"
		goroutines = 32
		perG       = 250
	)
	h := newVerbLabelledHistogram(metricName, "test", []float64{0.1, 1.0})

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < perG; j++ {
				h.Observe(verb, 0.05)
			}
		}()
	}
	close(start)
	wg.Wait()

	var buf bytes.Buffer
	h.Write(&buf)
	wantTotal := goroutines * perG
	wantCountLine := metricName + `_count{verb="` + verb + `"} ` + itoa(wantTotal)
	if !strings.Contains(buf.String(), wantCountLine) {
		t.Errorf("concurrent observe lost samples\nwant line: %s\n--- output ---\n%s",
			wantCountLine, buf.String())
	}
}

// itoa avoids importing strconv into the test for a single
// usage — keeps the deps minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
