package obs

// Pinned metric names for implementation-plan.md Stage 8.3.
//
// Operators scrape these names; the Grafana dashboard JSON at
// `deploy/dashboards/agent-memory.json` and the Prometheus
// rule file at `deploy/alerts/agent-memory.rules.yml` both
// hard-code them. Renaming a constant here is therefore a
// breaking change for the operator-facing surface; any rename
// MUST update the dashboard, the alert file, AND the
// dashboard / alert tests in lock-step.
//
// The naming follows the Prometheus convention: counter names
// end in `_total`, histogram base names end in `_seconds` (the
// unit), gauge names carry no suffix. The §8.3 brief uses
// `partition_provision_lag` (a duration) and
// `reranker_last_trained_at` (a timestamp) without the
// `_seconds` suffix; we mirror those names verbatim so the
// dashboard PromQL matches the brief, but the canonical
// in-binary registration of `reranker_last_trained_at_seconds`
// (see internal/rerankertrainer/metrics.go) ALSO survives —
// the suffix-less form is exported as an alias-only metric
// when the rerankertrainer binary renders /metrics.
const (
	// MetricRecallFilterUnpublishedTotal counts vector hits
	// that the §9.6a `RecallFilter` dropped because the
	// `embedding_publish.state` was not yet `published`.
	// Wired by `internal/embedding/filter.go`.
	MetricRecallFilterUnpublishedTotal = "recall_filter_unpublished_total"

	// MetricSpanUnresolvedTotal counts OTel spans the
	// Span Ingestor could not resolve to a known method/
	// block fingerprint. Wired by `internal/spaningestor`.
	MetricSpanUnresolvedTotal = "span_unresolved_total"

	// MetricTrainerCappedActorTotal counts reranker-trainer
	// per-actor row caps reached during a training tick.
	// Wired by `internal/rerankertrainer`.
	MetricTrainerCappedActorTotal = "trainer_capped_actor_total"

	// MetricMgmtIngestSpansTotal counts mgmt-api
	// `POST /v1/spans` calls partitioned by status and
	// repo_id. Wired by `internal/mgmtapi`.
	MetricMgmtIngestSpansTotal = "mgmt_ingest_spans_total"

	// MetricSnapshotPublishedTotal counts vector publishes
	// that reached the terminal `published` state. Wired by
	// `internal/embedding.Publisher`.
	MetricSnapshotPublishedTotal = "snapshot_published_total"

	// MetricObserveWALBufferDepth is the current depth of
	// the agent.observe WAL (episodes buffered to disk
	// pending writer recovery). Wired by `cmd/agent-api`.
	MetricObserveWALBufferDepth = "observe_wal_buffer_depth"

	// MetricConsolidatorEpisodeLag is the wall-clock
	// seconds between the newest Episode and the
	// Consolidator's high-water mark. Wired by
	// `internal/consolidator`.
	MetricConsolidatorEpisodeLag = "consolidator_episode_lag"
)

const (
	// MetricAgentRecallDurationSeconds is the histogram of
	// `agent.recall` round-trip durations. The §8.3 SLO is
	// p95 ≤ 1.5 s, p99 ≤ 4 s @ 50 RPS sustained.
	MetricAgentRecallDurationSeconds = "agent_recall_duration_seconds"

	// MetricAgentObserveDurationSeconds is the histogram of
	// `agent.observe` round-trip durations. The §8.3 SLO is
	// p95 ≤ 400 ms, p99 ≤ 1.5 s @ 50 RPS sustained.
	MetricAgentObserveDurationSeconds = "agent_observe_duration_seconds"

	// MetricAgentExpandDurationSeconds is the histogram of
	// `agent.expand` round-trip durations. The §8.3 SLO is
	// p95 ≤ 1.5 s, p99 ≤ 4 s @ 20 RPS sustained.
	MetricAgentExpandDurationSeconds = "agent_expand_duration_seconds"

	// MetricAgentSummarizeDurationSeconds is the histogram
	// of `agent.summarize` round-trip durations. The §8.3
	// SLO is p95 ≤ 4 s, p99 ≤ 10 s @ 5 RPS sustained.
	MetricAgentSummarizeDurationSeconds = "agent_summarize_duration_seconds"

	// MetricMgmtIngestSpansBatchDurationSeconds is the
	// histogram of mgmt-api `POST /v1/spans` batch
	// durations. The §8.3 SLO is p95 ≤ 2 s, p99 ≤ 5 s @
	// 50 batches/min.
	MetricMgmtIngestSpansBatchDurationSeconds = "mgmt_ingest_spans_batch_duration_seconds"
)

const (
	// MetricPartitionProvisionLag is the
	// stale-forward-partition gauge owned by Stage 8.2 (see
	// implementation-plan.md §8.2). The §8.3 work surfaces
	// the metric NAME to the dashboard / alert layer; the
	// underlying value is populated by the Stage 8.2
	// partition-rotation worker. The alert at
	// `deploy/alerts/agent-memory.rules.yml` uses
	// `absent(partition_provision_lag)` to make a missing
	// metric loud rather than silently green.
	MetricPartitionProvisionLag = "partition_provision_lag"

	// MetricRerankerLastTrainedAt is the §8.3 alias for the
	// reranker-trainer's `reranker_last_trained_at_seconds`
	// gauge. Exposing both names keeps the brief verbatim
	// (suffix-less) while preserving the existing canonical
	// metric the operator dashboards already scrape.
	MetricRerankerLastTrainedAt = "reranker_last_trained_at"

	// MetricRerankerLastTrainedAtSeconds is the canonical
	// name from `internal/rerankertrainer/metrics.go`.
	MetricRerankerLastTrainedAtSeconds = "reranker_last_trained_at_seconds"
)

const (
	// MetricQdrantBootstrapRunsTotal counts qdrant-bootstrap
	// runs partitioned by status (`success`/`failed`). Wired
	// by `cmd/qdrant-bootstrap`. Iter-2 evaluator fix #2:
	// the bootstrapper now exposes /metrics so its run
	// outcomes are visible alongside the other binaries.
	MetricQdrantBootstrapRunsTotal = "qdrant_bootstrap_runs_total"

	// MetricQdrantBootstrapDurationSeconds is the histogram
	// of bootstrap run durations (provision + optional
	// snapshot). Wired by `cmd/qdrant-bootstrap`.
	MetricQdrantBootstrapDurationSeconds = "qdrant_bootstrap_duration_seconds"

	// MetricQdrantBootstrapLastCompletedAt is the unix
	// timestamp (seconds) of the most-recent successful
	// bootstrap run. Wired by `cmd/qdrant-bootstrap`.
	// Operators alert on staleness as a "scheduler stuck"
	// signal in snapshot-loop mode.
	MetricQdrantBootstrapLastCompletedAt = "qdrant_bootstrap_last_completed_at"

	// MetricRerankerSidecarInferenceTotal counts cross-
	// encoder /rank requests served by the Python sidecar,
	// partitioned by status (`success`/`error`). Wired by
	// `cmd/reranker-sidecar/main.py`.
	MetricRerankerSidecarInferenceTotal = "reranker_sidecar_inference_total"

	// MetricRerankerSidecarInferenceDurationSeconds is the
	// histogram of /rank request durations on the Python
	// sidecar. Used by the dashboard to surface model
	// inference tail latency. Wired by `cmd/reranker-sidecar/main.py`.
	MetricRerankerSidecarInferenceDurationSeconds = "reranker_sidecar_inference_duration_seconds"

	// MetricWebhookReceiverRequestsTotal counts webhook-
	// receiver HTTP requests partitioned by status (one of
	// accepted/rejected_method/rejected_repo/rejected_body/
	// rejected_sig/rejected_kind/rejected_payload/error_db/
	// error_internal) and -- where available -- payload
	// kind. Iter-3 evaluator fix #2: webhook-receiver
	// previously exposed only an `up` gauge; this counter
	// surfaces the per-request status mix so the dashboard
	// and alerts can distinguish a misconfigured secret
	// (rejected_sig burst) from oversized payloads
	// (rejected_body burst) without grepping logs. Wired by
	// `internal/webhookreceiver/metrics.go`.
	MetricWebhookReceiverRequestsTotal = "webhook_receiver_requests_total"
)
