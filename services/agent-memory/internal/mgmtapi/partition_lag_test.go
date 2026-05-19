package mgmtapi

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestPartitionLagGauge_AllParentsAlwaysEmitted asserts the
// Write method renders one series per PartitionParents
// entry, even when the underlying query returns an empty
// map. This is the iter-2 evaluator fix #4 guarantee: the
// dashboard panel must never render `No data` even on a
// fresh fixture where pg_partman has not registered any
// parents.
func TestPartitionLagGauge_AllParentsAlwaysEmitted(t *testing.T) {
	t.Parallel()

	g := NewPartitionLagGauge(nil, nil).
		SetQueryFunc(func(_ context.Context, _ *sql.DB) (map[string]float64, error) {
			return nil, nil
		}).
		WithCacheTTL(0)

	var sb strings.Builder
	g.Write(&sb)
	out := sb.String()

	if !strings.Contains(out, "# TYPE partition_provision_lag gauge") {
		t.Fatalf("missing TYPE line:\n%s", out)
	}
	for _, parent := range PartitionParents {
		needle := `partition_provision_lag{parent="` + parent + `"} 0`
		if !strings.Contains(out, needle) {
			t.Errorf("missing series for parent %q; output was:\n%s", parent, out)
		}
	}
	if !strings.Contains(out, "mgmt_partition_lag_query_errors_total 0") {
		t.Errorf("error-counter series missing:\n%s", out)
	}
}

// TestPartitionLagGauge_QueryErrorIncrementsCounter asserts
// a refresh failure (e.g. pg_partman missing AND DB
// reachable but query rejected) increments the
// mgmt_partition_lag_query_errors_total counter while still
// emitting the per-parent gauge at zero so the dashboard
// keeps rendering.
func TestPartitionLagGauge_QueryErrorIncrementsCounter(t *testing.T) {
	t.Parallel()

	errSentinel := errors.New("partman missing")
	g := NewPartitionLagGauge(nil, nil).
		SetQueryFunc(func(_ context.Context, _ *sql.DB) (map[string]float64, error) {
			return nil, errSentinel
		}).
		WithCacheTTL(0)

	var sb strings.Builder
	g.Write(&sb)
	g.Write(&sb)

	if got := g.QueryErrorCount(); got != 2 {
		t.Fatalf("error counter should be 2 after two failed scrapes; got %d", got)
	}
	if !strings.Contains(sb.String(), "mgmt_partition_lag_query_errors_total 2") {
		t.Errorf("error counter not rendered:\n%s", sb.String())
	}
}

// TestPartitionLagGauge_PerParentValueRendered confirms a
// non-zero lag for a single parent renders correctly while
// the others stay at zero.
func TestPartitionLagGauge_PerParentValueRendered(t *testing.T) {
	t.Parallel()

	g := NewPartitionLagGauge(nil, nil).
		SetQueryFunc(func(_ context.Context, _ *sql.DB) (map[string]float64, error) {
			return map[string]float64{
				"episode": 90123,
			}, nil
		}).
		WithCacheTTL(0)

	var sb strings.Builder
	g.Write(&sb)
	out := sb.String()

	if !strings.Contains(out, `partition_provision_lag{parent="episode"} 90123`) {
		t.Errorf("episode parent should render the supplied lag value:\n%s", out)
	}
	if !strings.Contains(out, `partition_provision_lag{parent="trace_observation_log"} 0`) {
		t.Errorf("unconfigured parent should default to zero:\n%s", out)
	}
}

// TestPartitionLagGauge_CacheTTLAvoidsRedundantQueries
// asserts back-to-back scrapes within the cache window do
// not hit the underlying query function more than once.
func TestPartitionLagGauge_CacheTTLAvoidsRedundantQueries(t *testing.T) {
	t.Parallel()

	var calls int
	g := NewPartitionLagGauge(nil, nil).
		SetQueryFunc(func(_ context.Context, _ *sql.DB) (map[string]float64, error) {
			calls++
			return map[string]float64{"episode": 10}, nil
		}).
		WithCacheTTL(time.Minute)

	var sink strings.Builder
	g.Write(&sink)
	g.Write(&sink)
	g.Write(&sink)
	if calls != 1 {
		t.Fatalf("expected exactly 1 query call with 1m cache TTL; got %d", calls)
	}
}

// TestPartitionLagGauge_PreservesPreviousValuesOnError is
// the iter-3 evaluator fix #6: a transient query failure
// MUST NOT clobber a real high-lag reading with zeros.
// First scrape succeeds with episode=90123; the second
// query fails. The third scrape must still emit episode=90123
// (the last-known-good value) rather than zero, otherwise an
// in-progress incident would silently turn green.
func TestPartitionLagGauge_PreservesPreviousValuesOnError(t *testing.T) {
	t.Parallel()

	var callIdx int
	g := NewPartitionLagGauge(nil, nil).
		SetQueryFunc(func(_ context.Context, _ *sql.DB) (map[string]float64, error) {
			callIdx++
			if callIdx == 1 {
				return map[string]float64{"episode": 90123}, nil
			}
			return nil, errors.New("transient DB blip")
		}).
		WithCacheTTL(0)

	// First scrape: successful, sets the cache.
	var first strings.Builder
	g.Write(&first)
	if !strings.Contains(first.String(),
		`partition_provision_lag{parent="episode"} 90123`) {
		t.Fatalf("first scrape should render episode=90123:\n%s", first.String())
	}

	// Second scrape: fails. Must still render the prior
	// high-lag value, not zero.
	var second strings.Builder
	g.Write(&second)
	if !strings.Contains(second.String(),
		`partition_provision_lag{parent="episode"} 90123`) {
		t.Errorf("second scrape MUST preserve last-known-good value 90123 on query error; got:\n%s",
			second.String())
	}
	if !strings.Contains(second.String(),
		"mgmt_partition_lag_query_errors_total 1") {
		t.Errorf("error counter should tick on the failed scrape; got:\n%s",
			second.String())
	}
}

// TestPartitionLagGauge_ZeroFillOnInitialError covers the
// boot-with-pg_partman-unreachable case: when there is NO
// prior snapshot AND the query fails, we still emit zeros
// per parent so the dashboard renders rather than going to
// "No data". The error counter still ticks so operators see
// the underlying failure.
func TestPartitionLagGauge_ZeroFillOnInitialError(t *testing.T) {
	t.Parallel()

	g := NewPartitionLagGauge(nil, nil).
		SetQueryFunc(func(_ context.Context, _ *sql.DB) (map[string]float64, error) {
			return nil, errors.New("pg_partman not installed")
		}).
		WithCacheTTL(0)

	var sb strings.Builder
	g.Write(&sb)
	out := sb.String()
	for _, parent := range PartitionParents {
		needle := `partition_provision_lag{parent="` + parent + `"} 0`
		if !strings.Contains(out, needle) {
			t.Errorf("first scrape with no prior data MUST zero-fill parent %q; got:\n%s",
				parent, out)
		}
	}
	if !strings.Contains(out, "mgmt_partition_lag_query_errors_total 1") {
		t.Errorf("error counter should tick:\n%s", out)
	}
}

// TestFormatFloat_PromtextShape verifies the
// formatFloat helper matches the Prometheus exposition
// expectations callers rely on.
func TestFormatFloat_PromtextShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0"},
		{1, "1"},
		{90123, "90123"},
		{1.5, "1.500000"},
	}
	for _, c := range cases {
		if got := formatFloat(c.in); got != c.want {
			t.Errorf("formatFloat(%v) = %q; want %q", c.in, got, c.want)
		}
	}
}
