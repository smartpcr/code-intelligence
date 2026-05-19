package partitionmaintainer

import "sync/atomic"

// Metric name constants. The binary's /metrics endpoint and any
// downstream Prometheus scrape keys off these literals.
// Centralised here so a typo in one call site does not silently
// un-pair a counter from its exposition.
const (
	// MetricPartitionMaintenanceRunsTotal counts every
	// RunMaintenance invocation (success OR failure). Together
	// with MetricPartitionMaintenanceErrorsTotal this gives a
	// per-binary failure rate. A scheduler stalled at the loop
	// level surfaces as "time since last increment of this
	// counter > MaintenanceInterval" -- the operator alert for
	// a hung process; the Stage 8.2 partition_provision_lag
	// gauge below catches the data-layer consequence.
	MetricPartitionMaintenanceRunsTotal = "partition_maintenance_runs_total"

	// MetricPartitionMaintenanceErrorsTotal counts only the
	// RunMaintenance invocations that surfaced a non-nil error.
	MetricPartitionMaintenanceErrorsTotal = "partition_maintenance_errors_total"

	// MetricPartitionLagScrapesTotal counts every ScrapeLag
	// invocation (success OR failure).
	MetricPartitionLagScrapesTotal = "partition_provision_lag_scrapes_total"

	// MetricPartitionLagScrapeErrorsTotal counts the failed
	// ScrapeLag invocations.
	MetricPartitionLagScrapeErrorsTotal = "partition_provision_lag_scrape_errors_total"

	// MetricPartitionProvisionLagSeconds is the implementation-
	// plan §8.2 GAUGE: the maximum across in-scope parents of
	// `max(0, (now() + 1 day) - latest_child_end_time)`. A
	// healthy steady-state value is 0; the Stage 8.2 alert
	// rule (`deploy/local/prometheus/rules/partition_rotation.rules.yml`)
	// fires when this gauge exceeds 86400 (1 day) for ≥10
	// minutes. The EXPORTED metric name is the spec literal
	// `partition_provision_lag` (no `_seconds` suffix even
	// though the value IS in seconds -- the spec keeps the
	// shorter form for grep-ability with implementation-plan
	// §8.2). The Go symbol keeps `Seconds` for in-code clarity.
	// Stored as a uint64 of seconds for atomic
	// compare-and-swap without an extra mutex.
	MetricPartitionProvisionLagSeconds = "partition_provision_lag"

	// MetricPartitionParentsObservedTotal is the count of
	// in-scope parents the most recent ScrapeLag iterated over.
	// Surfaced as a gauge so an operator can spot a scrape that
	// suddenly observes 0 parents (which would silently mask a
	// lag spike on the gauge above). Stored separately from the
	// success counter so it does not reset on a transient
	// scrape failure.
	MetricPartitionParentsObservedGauge = "partition_provision_parents_observed"
)

// Metrics is the package counter / gauge surface. Construct via
// NewMetrics(); read via Snapshot(). All accessors are
// goroutine-safe via sync/atomic so the Run-loop goroutines
// (maintenance + scrape) and a future /metrics scrape goroutine
// can interleave without locking.
type Metrics struct {
	maintenanceRuns   atomic.Uint64
	maintenanceErrors atomic.Uint64
	lagScrapes        atomic.Uint64
	lagScrapeErrors   atomic.Uint64
	// lagSeconds is a GAUGE -- the most-recent observed lag in
	// whole seconds. Stored as uint64 (the lag is clamped to >=
	// 0 before storing) so atomic.Store/Load is sufficient.
	lagSeconds atomic.Uint64
	// parentsObserved is the count of in-scope parents the
	// most recent ScrapeLag iterated over.
	parentsObserved atomic.Uint64
}

// NewMetrics returns a zero-initialised Metrics.
func NewMetrics() *Metrics {
	return &Metrics{}
}

// IncMaintenanceRuns increments the per-run counter. Called by
// Service.RunMaintenance once per invocation, regardless of
// outcome.
func (m *Metrics) IncMaintenanceRuns() { m.maintenanceRuns.Add(1) }

// IncMaintenanceErrors increments the per-error counter. Called
// by Service.RunMaintenance when the underlying SQL call returns
// a non-nil error.
func (m *Metrics) IncMaintenanceErrors() { m.maintenanceErrors.Add(1) }

// IncLagScrapes increments the lag-scrape counter. Called by
// Service.ScrapeLag once per invocation, regardless of outcome.
func (m *Metrics) IncLagScrapes() { m.lagScrapes.Add(1) }

// IncLagScrapeErrors increments the lag-scrape error counter.
func (m *Metrics) IncLagScrapeErrors() { m.lagScrapeErrors.Add(1) }

// SetProvisionLagSeconds atomically replaces the gauge value.
// Called by Service.ScrapeLag with the MAX-across-parents lag
// value (clamped to >= 0 by the caller).
func (m *Metrics) SetProvisionLagSeconds(seconds uint64) {
	m.lagSeconds.Store(seconds)
}

// SetParentsObserved atomically replaces the per-scrape gauge.
func (m *Metrics) SetParentsObserved(n uint64) {
	m.parentsObserved.Store(n)
}

// MaintenanceRunsTotal returns the current value of the
// partition_maintenance_runs_total counter.
func (m *Metrics) MaintenanceRunsTotal() uint64 { return m.maintenanceRuns.Load() }

// MaintenanceErrorsTotal returns the current value of the
// partition_maintenance_errors_total counter.
func (m *Metrics) MaintenanceErrorsTotal() uint64 { return m.maintenanceErrors.Load() }

// LagScrapesTotal returns the partition_provision_lag_scrapes_total
// value.
func (m *Metrics) LagScrapesTotal() uint64 { return m.lagScrapes.Load() }

// LagScrapeErrorsTotal returns the
// partition_provision_lag_scrape_errors_total value.
func (m *Metrics) LagScrapeErrorsTotal() uint64 { return m.lagScrapeErrors.Load() }

// ProvisionLagSeconds returns the current gauge value (whole
// seconds). The Stage 8.2 alert rule keys off this.
func (m *Metrics) ProvisionLagSeconds() uint64 { return m.lagSeconds.Load() }

// ParentsObserved returns the count of parents the most recent
// ScrapeLag iterated over.
func (m *Metrics) ParentsObserved() uint64 { return m.parentsObserved.Load() }

// Snapshot returns a stable map of every exposed counter /
// gauge keyed by the Metric* constants. Freshly allocated per
// call so a caller can iterate over it without holding a lock
// against concurrent IncX / SetX writers.
func (m *Metrics) Snapshot() map[string]uint64 {
	return map[string]uint64{
		MetricPartitionMaintenanceRunsTotal:   m.maintenanceRuns.Load(),
		MetricPartitionMaintenanceErrorsTotal: m.maintenanceErrors.Load(),
		MetricPartitionLagScrapesTotal:        m.lagScrapes.Load(),
		MetricPartitionLagScrapeErrorsTotal:   m.lagScrapeErrors.Load(),
		MetricPartitionProvisionLagSeconds:    m.lagSeconds.Load(),
		MetricPartitionParentsObservedGauge:   m.parentsObserved.Load(),
	}
}
