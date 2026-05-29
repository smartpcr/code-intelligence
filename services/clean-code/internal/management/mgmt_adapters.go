package management

// Stage 3.4 -- composition-root adapters that bridge the
// concrete [metric_ingestor.RetractDispatcher] /
// [metric_ingestor.RescanEnqueuer] types onto the narrow
// management-side interfaces ([RetractDispatcher],
// [RescanEnqueuer], [SampleResolver]).
//
// # Why a separate file
//
// The handlers in [mgmt_verbs.go] are written against the
// management-side interfaces so tests can inject pure-Go
// fakes without dragging the metric_ingestor package
// (which itself depends on repo_indexer + uuid + the
// foundation-dispatch tree) into the test binary's import
// graph.
//
// The composition root, however, has all those deps wired
// already. This file holds the small concrete adapter types
// that translate between the management-side flat-argument
// interface and the metric_ingestor request-struct shape.
//
// Production wiring:
//
//	dispatcher := metric_ingestor.NewRetractDispatcher(...)
//	enqueuer   := metric_ingestor.NewRescanEnqueuer(...)
//	store      := metric_ingestor.NewInMemoryRetractStore() // or PG store
//	writer := management.NewMgmtWriter(
//	    store, // SampleResolver
//	    management.AdaptMetricIngestorRetractDispatcher(dispatcher),
//	    management.AdaptMetricIngestorRescanEnqueuer(enqueuer),
//	    appender,
//	)
//	http.Handle("/", writer.Routes())

import (
	"context"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
)

// retractDispatcherAdapter wraps a concrete
// [*metric_ingestor.RetractDispatcher] so it satisfies the
// management-side [RetractDispatcher] interface.
//
// The adapter is intentionally STATELESS -- one instance can
// be reused across requests because the underlying dispatcher
// is itself stateless past its dependencies.
type retractDispatcherAdapter struct {
	inner *metric_ingestor.RetractDispatcher
}

// AdaptMetricIngestorRetractDispatcher returns an adapter
// that exposes `d` as a management-side [RetractDispatcher].
// `d` MUST be non-nil; the composition root caller is
// responsible for that pre-check (the management
// [NewMgmtWriter] PERMITS nil to enable scaffold mode, so
// "nil-but-wrapped" would defeat the 503 path).
func AdaptMetricIngestorRetractDispatcher(d *metric_ingestor.RetractDispatcher) RetractDispatcher {
	if d == nil {
		return nil
	}
	return &retractDispatcherAdapter{inner: d}
}

// Dispatch implements [RetractDispatcher].
func (a *retractDispatcherAdapter) Dispatch(ctx context.Context, sampleID uuid.UUID, reason, appendedBy string) (RetractResult, error) {
	res, err := a.inner.Dispatch(ctx, metric_ingestor.RetractRequest{
		SampleID:   sampleID,
		Reason:     reason,
		AppendedBy: appendedBy,
	})
	if err != nil {
		// Preserve the underlying error so the wire-layer
		// `writeRetractDispatchError` can still inspect the
		// chain (e.g. `errors.Is(err,
		// metric_ingestor.ErrRetractUnknownSample)` would
		// hit). We return a zero RetractResult on the error
		// path -- mirrors the dispatcher's own contract.
		return RetractResult{}, err
	}
	return RetractResult{
		Retraction: RetractionRow{
			RetractionID: res.Retraction.RetractionID,
			SampleID:     res.Retraction.SampleID,
			Reason:       res.Retraction.Reason,
			AppendedBy:   res.Retraction.AppendedBy,
			CreatedAt:    res.Retraction.CreatedAt,
		},
		ScanRunID: res.ScanRunID,
		Inserted:  res.Inserted,
	}, nil
}

// rescanEnqueuerAdapter wraps a concrete
// [*metric_ingestor.RescanEnqueuer] so it satisfies the
// management-side [RescanEnqueuer] interface.
type rescanEnqueuerAdapter struct {
	inner *metric_ingestor.RescanEnqueuer
}

// AdaptMetricIngestorRescanEnqueuer returns an adapter that
// exposes `e` as a management-side [RescanEnqueuer]. `e`
// MUST be non-nil; nil is preserved (returns nil) so the
// management 503 path still fires.
func AdaptMetricIngestorRescanEnqueuer(e *metric_ingestor.RescanEnqueuer) RescanEnqueuer {
	if e == nil {
		return nil
	}
	return &rescanEnqueuerAdapter{inner: e}
}

// Enqueue implements [RescanEnqueuer].
func (a *rescanEnqueuerAdapter) Enqueue(ctx context.Context, repoID uuid.UUID, sha, requestedBy string) (RescanResult, error) {
	res, err := a.inner.Enqueue(ctx, metric_ingestor.RescanRequest{
		RepoID:      repoID,
		SHA:         sha,
		RequestedBy: requestedBy,
	})
	if err != nil {
		return RescanResult{}, err
	}
	return RescanResult{
		ScanRunID:   res.ScanRunID,
		RepoID:      res.RepoID,
		SHA:         res.SHA,
		RequestedBy: res.RequestedBy,
		OpenedAt:    res.OpenedAt,
	}, nil
}

// Compile-time guards.
var (
	_ RetractDispatcher = (*retractDispatcherAdapter)(nil)
	_ RescanEnqueuer    = (*rescanEnqueuerAdapter)(nil)
)
