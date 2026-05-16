package tracelogpruner

import "sync/atomic"

// Metric name constants -- the binary's /metrics endpoint and
// any downstream collector key off these literals. Centralised
// here so a typo in one call site does not silently un-pair a
// counter from its exposition.
const (
	// MetricTraceLogPartitionsDroppedTotal is the
	// implementation-plan Stage 4.3 counter for the number of
	// `trace_observation_log` partitions detached by the pruner
	// across the binary's lifetime. Emitted as a Prometheus
	// counter (monotonically increasing across runs of the
	// binary; reset to zero only by binary restart).
	MetricTraceLogPartitionsDroppedTotal = "trace_log_partitions_dropped_total"

	// MetricTraceLogPruneRunsTotal counts every Prune invocation
	// (success OR failure) so an alerting rule can detect a
	// stuck cron from the time-since-last-increment of this
	// counter. Distinct from the dropped-partitions counter:
	// the latter can be zero on a healthy day when no partition
	// has fallen out of the retention window yet.
	MetricTraceLogPruneRunsTotal = "trace_log_prune_runs_total"

	// MetricTraceLogPruneErrorsTotal counts only the Prune
	// invocations that surfaced a non-nil error. Together with
	// MetricTraceLogPruneRunsTotal this gives a per-binary
	// failure rate without parsing logs.
	MetricTraceLogPruneErrorsTotal = "trace_log_prune_errors_total"
)

// Metrics is the package's atomic-counter surface. Construct
// via NewMetrics(); read via Snapshot(). The counters are
// goroutine-safe via sync/atomic so the Run-loop goroutine and
// a future scrape goroutine can read concurrently without
// locking.
type Metrics struct {
	partitionsDropped atomic.Uint64
	runs              atomic.Uint64
	errors            atomic.Uint64
}

// NewMetrics returns a zero-initialised Metrics.
func NewMetrics() *Metrics {
	return &Metrics{}
}

// IncPartitionsDropped adds `n` to the partitions-dropped
// counter. Called by Service.Prune with the integer return
// value of `partman.drop_partition_time` (which is the count
// of partitions detached or dropped on this invocation).
func (m *Metrics) IncPartitionsDropped(n uint64) {
	m.partitionsDropped.Add(n)
}

// IncRuns increments the per-run counter. Called by
// Service.Prune once per invocation, regardless of outcome.
func (m *Metrics) IncRuns() {
	m.runs.Add(1)
}

// IncErrors increments the per-error counter. Called by
// Service.Prune when the underlying SQL call returns a non-nil
// error.
func (m *Metrics) IncErrors() {
	m.errors.Add(1)
}

// PartitionsDroppedTotal returns the current value of the
// trace_log_partitions_dropped_total counter. Used by the
// integration test to assert the Stage 4.3 metric contract
// without parsing the binary's /metrics exposition.
func (m *Metrics) PartitionsDroppedTotal() uint64 {
	return m.partitionsDropped.Load()
}

// RunsTotal returns the trace_log_prune_runs_total value.
func (m *Metrics) RunsTotal() uint64 {
	return m.runs.Load()
}

// ErrorsTotal returns the trace_log_prune_errors_total value.
func (m *Metrics) ErrorsTotal() uint64 {
	return m.errors.Load()
}

// Snapshot returns a stable map of the package's counters
// keyed by the Metric* constants. The map is freshly allocated
// per call so a caller can iterate over it without holding a
// lock against concurrent IncX writers.
func (m *Metrics) Snapshot() map[string]uint64 {
	return map[string]uint64{
		MetricTraceLogPartitionsDroppedTotal: m.partitionsDropped.Load(),
		MetricTraceLogPruneRunsTotal:         m.runs.Load(),
		MetricTraceLogPruneErrorsTotal:       m.errors.Load(),
	}
}
