package obs_test

import (
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
)

// TestMetricNames_MatchStage_8_3_Brief is the single source of
// truth that the §8.3 metric NAMES exported from this package
// match the implementation-plan.md Stage 8.3 brief verbatim.
// A regression here is operator-visible: the dashboard /
// alert rules in `deploy/{dashboards,alerts}/` reference these
// strings, and Prometheus would silently emit a renamed
// time-series instead of failing the build.
func TestMetricNames_MatchStage_8_3_Brief(t *testing.T) {
	cases := map[string]string{
		"recall_filter_unpublished_total":           obs.MetricRecallFilterUnpublishedTotal,
		"span_unresolved_total":                     obs.MetricSpanUnresolvedTotal,
		"trainer_capped_actor_total":                obs.MetricTrainerCappedActorTotal,
		"mgmt_ingest_spans_total":                   obs.MetricMgmtIngestSpansTotal,
		"snapshot_published_total":                  obs.MetricSnapshotPublishedTotal,
		"observe_wal_buffer_depth":                  obs.MetricObserveWALBufferDepth,
		"consolidator_episode_lag":                  obs.MetricConsolidatorEpisodeLag,
		"agent_recall_duration_seconds":             obs.MetricAgentRecallDurationSeconds,
		"agent_observe_duration_seconds":            obs.MetricAgentObserveDurationSeconds,
		"agent_expand_duration_seconds":             obs.MetricAgentExpandDurationSeconds,
		"agent_summarize_duration_seconds":          obs.MetricAgentSummarizeDurationSeconds,
		"mgmt_ingest_spans_batch_duration_seconds":  obs.MetricMgmtIngestSpansBatchDurationSeconds,
		"partition_provision_lag":                   obs.MetricPartitionProvisionLag,
		"reranker_last_trained_at":                  obs.MetricRerankerLastTrainedAt,
	}
	for want, got := range cases {
		if got != want {
			t.Errorf("metric constant differs from §8.3 brief: want %q, got %q", want, got)
		}
	}
}

// TestMetricNames_FollowPromConvention sanity-checks naming.
// Histograms end in `_seconds`; counters end in `_total`;
// gauges have neither suffix.
func TestMetricNames_FollowPromConvention(t *testing.T) {
	counters := []string{
		obs.MetricRecallFilterUnpublishedTotal,
		obs.MetricSpanUnresolvedTotal,
		obs.MetricTrainerCappedActorTotal,
		obs.MetricMgmtIngestSpansTotal,
		obs.MetricSnapshotPublishedTotal,
	}
	for _, name := range counters {
		if !strings.HasSuffix(name, "_total") {
			t.Errorf("counter %q must end in _total", name)
		}
	}
	histograms := []string{
		obs.MetricAgentRecallDurationSeconds,
		obs.MetricAgentObserveDurationSeconds,
		obs.MetricAgentExpandDurationSeconds,
		obs.MetricAgentSummarizeDurationSeconds,
		obs.MetricMgmtIngestSpansBatchDurationSeconds,
	}
	for _, name := range histograms {
		if !strings.HasSuffix(name, "_duration_seconds") {
			t.Errorf("histogram %q must end in _duration_seconds", name)
		}
	}
}
