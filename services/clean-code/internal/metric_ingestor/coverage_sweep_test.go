package metric_ingestor_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/ast/scope"
	"forge/services/clean-code/internal/ingest/coverage"
	"forge/services/clean-code/internal/metric_ingestor"
	"forge/services/clean-code/internal/metrics/recipes"
)

func mustParseUUIDForSweep(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.FromString(s)
	if err != nil {
		t.Fatalf("uuid.FromString(%q): %v", s, err)
	}
	return u
}

const coverageFixtureXML = `<?xml version="1.0" ?>
<coverage line-rate="0.75" branch-rate="0.5">
  <packages>
    <package name="internal.svc">
      <classes>
        <class name="Foo" filename="internal/svc/handler.go">
          <lines>
            <line number="1" hits="1" branch="false"/>
            <line number="2" hits="0" branch="true" condition-coverage="50% (1/2)"/>
          </lines>
        </class>
      </classes>
    </package>
  </packages>
</coverage>`

// newCoverageSweepFixture wires a CoverageSweep with an
// in-memory writer and a resolver populated for the fixture
// XML above. Returns the sweep, writer, repoID, sha, and
// a deterministic scope_id for assertions.
func newCoverageSweepFixture(t *testing.T) (*metric_ingestor.CoverageSweep, *metric_ingestor.InMemoryMetricSampleWriter, uuid.UUID, string, uuid.UUID) {
	t.Helper()
	repoID := mustParseUUIDForSweep(t, "11111111-1111-1111-1111-111111111111")
	scopeID := mustParseUUIDForSweep(t, "22222222-2222-2222-2222-222222222222")
	sha := strings.Repeat("a", 40)
	res := coverage.NewMapScopeResolver()
	res.Add(repoID, "internal/svc/handler.go", scopeID, recipes.ScopeRef{
		Kind:          scope.KindFile,
		Path:          "internal/svc/handler.go",
		QualifiedName: "internal/svc/handler.go",
	})
	hyd := coverage.NewHydrator(res)
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewCoverageSweep(hyd, writer)
	return sweep, writer, repoID, sha, scopeID
}

func TestCoverageSweep_HappyPath_StampsProducerRunID(t *testing.T) {
	t.Parallel()
	sweep, writer, repoID, sha, scopeID := newCoverageSweepFixture(t)

	payload, err := coverage.ParseXML([]byte(coverageFixtureXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	scanRunID := mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333")
	scanRun := metric_ingestor.ScanRunContext{
		ID:     scanRunID,
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: repoID,
		SHA:    sha,
	}
	res, err := sweep.Run(context.Background(), scanRun, payload)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// One file resolves to two rows (line + branch ratio).
	if res.SamplesWritten != 2 {
		t.Errorf("SamplesWritten = %d; want 2", res.SamplesWritten)
	}
	if res.RowsHydrated != 2 {
		t.Errorf("RowsHydrated = %d; want 2", res.RowsHydrated)
	}
	if res.SkippedUnboundScopeCount != 0 {
		t.Errorf("SkippedUnboundScopeCount = %d; want 0", res.SkippedUnboundScopeCount)
	}
	records := writer.Records()
	if len(records) != 2 {
		t.Fatalf("writer.Records: want 2, got %d", len(records))
	}
	for i, r := range records {
		if r.ProducerRunID != scanRunID {
			t.Errorf("records[%d].ProducerRunID = %s; want %s", i, r.ProducerRunID, scanRunID)
		}
		if r.RepoID != repoID {
			t.Errorf("records[%d].RepoID = %s; want %s", i, r.RepoID, repoID)
		}
		if r.SHA != sha {
			t.Errorf("records[%d].SHA = %q; want %q (single-SHA binding)", i, r.SHA, sha)
		}
		if r.ScopeID != scopeID {
			t.Errorf("records[%d].ScopeID = %s; want %s", i, r.ScopeID, scopeID)
		}
		if r.Pack != recipes.PackIngested {
			t.Errorf("records[%d].Pack = %q; want %q", i, r.Pack, recipes.PackIngested)
		}
		if r.Source != recipes.SourceIngested {
			t.Errorf("records[%d].Source = %q; want %q", i, r.Source, recipes.SourceIngested)
		}
		if r.MetricVersion != coverage.MetricVersion {
			t.Errorf("records[%d].MetricVersion = %d; want %d", i, r.MetricVersion, coverage.MetricVersion)
		}
		if r.SampleID == uuid.Nil {
			t.Errorf("records[%d].SampleID is the zero UUID (mint must populate)", i)
		}
	}
}

func TestCoverageSweep_RejectsNonExternalSingleKind(t *testing.T) {
	t.Parallel()
	sweep, _, repoID, sha, _ := newCoverageSweepFixture(t)
	payload, err := coverage.ParseXML([]byte(coverageFixtureXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	for _, k := range []string{
		metric_ingestor.ScanRunKindFull,
		metric_ingestor.ScanRunKindDelta,
		metric_ingestor.ScanRunKindExternalPerRow,
		metric_ingestor.ScanRunKindRetract,
	} {
		t.Run(k, func(t *testing.T) {
			scanRun := metric_ingestor.ScanRunContext{
				ID:     mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333"),
				Kind:   k,
				RepoID: repoID,
				SHA:    sha,
			}
			_, err := sweep.Run(context.Background(), scanRun, payload)
			if err == nil {
				t.Fatalf("Run: want error for kind=%q, got nil", k)
			}
			if !errors.Is(err, metric_ingestor.ErrInvalidScanRunKind) {
				t.Errorf("err = %v; want errors.Is ErrInvalidScanRunKind", err)
			}
		})
	}
}

func TestCoverageSweep_RejectsSHAMismatch(t *testing.T) {
	t.Parallel()
	sweep, _, repoID, sha, _ := newCoverageSweepFixture(t)
	payload, err := coverage.ParseXML([]byte(coverageFixtureXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	scanRun := metric_ingestor.ScanRunContext{
		ID:     mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333"),
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: repoID,
		// Deliberate mismatch: payload built with `sha`, the
		// ScanRun reports a different commit. Single-SHA
		// binding requires the two channels to agree.
		SHA: strings.Repeat("e", 40),
	}
	_, err = sweep.Run(context.Background(), scanRun, payload)
	if err == nil {
		t.Fatalf("Run: want ErrCoverageSHAMismatch, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrCoverageSHAMismatch) {
		t.Errorf("err = %v; want errors.Is ErrCoverageSHAMismatch", err)
	}
}

func TestCoverageSweep_RejectsRepoIDMismatch(t *testing.T) {
	t.Parallel()
	sweep, _, repoID, sha, _ := newCoverageSweepFixture(t)
	payload, err := coverage.ParseXML([]byte(coverageFixtureXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	scanRun := metric_ingestor.ScanRunContext{
		ID:     mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333"),
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: mustParseUUIDForSweep(t, "99999999-9999-9999-9999-999999999999"),
		SHA:    sha,
	}
	_, err = sweep.Run(context.Background(), scanRun, payload)
	if err == nil {
		t.Fatalf("Run: want ErrRepoIDMismatch, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrRepoIDMismatch) {
		t.Errorf("err = %v; want errors.Is ErrRepoIDMismatch", err)
	}
}

func TestCoverageSweep_RejectsNilPayload(t *testing.T) {
	t.Parallel()
	sweep, _, repoID, sha, _ := newCoverageSweepFixture(t)
	scanRun := metric_ingestor.ScanRunContext{
		ID:     mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333"),
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: repoID,
		SHA:    sha,
	}
	_, err := sweep.Run(context.Background(), scanRun, nil)
	if err == nil {
		t.Fatalf("Run: want error for nil payload, got nil")
	}
}

func TestCoverageSweep_WriterFailurePropagates(t *testing.T) {
	t.Parallel()
	sweep, writer, repoID, sha, _ := newCoverageSweepFixture(t)
	writer.FailNext(errors.New("pg connection refused"))

	payload, err := coverage.ParseXML([]byte(coverageFixtureXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	scanRun := metric_ingestor.ScanRunContext{
		ID:     mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333"),
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: repoID,
		SHA:    sha,
	}
	_, err = sweep.Run(context.Background(), scanRun, payload)
	if err == nil {
		t.Fatalf("Run: want ErrWriterFailure, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrWriterFailure) {
		t.Errorf("err = %v; want errors.Is ErrWriterFailure", err)
	}
}

func TestCoverageSweep_CountsSkippedUnboundScope(t *testing.T) {
	t.Parallel()
	// Fixture resolver knows internal/svc/handler.go only;
	// the XML below references an additional helper.go that
	// must land in the skip bucket.
	repoID := mustParseUUIDForSweep(t, "11111111-1111-1111-1111-111111111111")
	scopeID := mustParseUUIDForSweep(t, "22222222-2222-2222-2222-222222222222")
	res := coverage.NewMapScopeResolver()
	res.Add(repoID, "internal/svc/handler.go", scopeID, recipes.ScopeRef{
		Kind:          scope.KindFile,
		Path:          "internal/svc/handler.go",
		QualifiedName: "internal/svc/handler.go",
	})
	hyd := coverage.NewHydrator(res)
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewCoverageSweep(hyd, writer)

	body := `<?xml version="1.0" ?>
<coverage>
  <packages><package><classes>
    <class filename="internal/svc/handler.go"><lines><line number="1" hits="1"/></lines></class>
    <class filename="internal/util/helper.go"><lines><line number="1" hits="1"/></lines></class>
  </classes></package></packages>
</coverage>`
	sha := strings.Repeat("b", 40)
	payload, err := coverage.ParseXML([]byte(body), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	scanRun := metric_ingestor.ScanRunContext{
		ID:     mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333"),
		Kind:   metric_ingestor.ScanRunKindExternalSingle,
		RepoID: repoID,
		SHA:    sha,
	}
	out, err := sweep.Run(context.Background(), scanRun, payload)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.SkippedUnboundScopeCount != 1 {
		t.Errorf("SkippedUnboundScopeCount = %d; want 1", out.SkippedUnboundScopeCount)
	}
	if out.SamplesWritten != 1 {
		t.Errorf("SamplesWritten = %d; want 1 (only handler.go landed)", out.SamplesWritten)
	}
}

func TestNewCoverageSweep_PanicsOnNilArgs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func()
	}{
		{
			name: "nil Hydrator",
			fn: func() {
				_ = metric_ingestor.NewCoverageSweep(nil, metric_ingestor.NewInMemoryMetricSampleWriter())
			},
		},
		{
			name: "nil Writer",
			fn: func() {
				_ = metric_ingestor.NewCoverageSweep(coverage.NewHydrator(coverage.NewMapScopeResolver()), nil)
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("NewCoverageSweep did not panic for %s", tc.name)
				}
			}()
			tc.fn()
		})
	}
}

func TestIngestor_ExternalSingle_RequiresCoveragePayload(t *testing.T) {
	t.Parallel()
	sweep, _, repoID, _, _ := newCoverageSweepFixture(t)
	churnSweep, _, _ := newSweep(t, 90, nil)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, churnSweep).
		WithCoverageSweep(sweep)

	_, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: metric_ingestor.ScanRunContext{
			ID:     mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333"),
			Kind:   metric_ingestor.ScanRunKindExternalSingle,
			RepoID: repoID,
		},
		// Coverage left nil deliberately.
	})
	if err == nil {
		t.Fatalf("Run: want ErrMissingCoveragePayloadForExternalSingle, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrMissingCoveragePayloadForExternalSingle) {
		t.Errorf("err = %v; want errors.Is ErrMissingCoveragePayloadForExternalSingle", err)
	}
}

func TestIngestor_ExternalSingle_UnwiredSweepErrors(t *testing.T) {
	t.Parallel()
	_, _, repoID, sha, _ := newCoverageSweepFixture(t)
	payload, err := coverage.ParseXML([]byte(coverageFixtureXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	churnSweep, _, _ := newSweep(t, 90, nil)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, churnSweep)
	// Deliberately do NOT WithCoverageSweep.

	_, err = ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: metric_ingestor.ScanRunContext{
			ID:     mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333"),
			Kind:   metric_ingestor.ScanRunKindExternalSingle,
			RepoID: repoID,
		},
		Coverage: payload,
	})
	if err == nil {
		t.Fatalf("Run: want ErrCoverageSweepUnwired, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrCoverageSweepUnwired) {
		t.Errorf("err = %v; want errors.Is ErrCoverageSweepUnwired", err)
	}
}

func TestIngestor_ExternalSingle_HappyPathReportsCoverageCounters(t *testing.T) {
	t.Parallel()
	sweep, _, repoID, sha, _ := newCoverageSweepFixture(t)
	payload, err := coverage.ParseXML([]byte(coverageFixtureXML), repoID, sha)
	if err != nil {
		t.Fatalf("ParseXML: %v", err)
	}
	churnSweep, _, _ := newSweep(t, 90, nil)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, churnSweep).
		WithCoverageSweep(sweep)

	out, err := ing.Run(context.Background(), metric_ingestor.RunRequest{
		ScanRun: metric_ingestor.ScanRunContext{
			ID:     mustParseUUIDForSweep(t, "33333333-3333-3333-3333-333333333333"),
			Kind:   metric_ingestor.ScanRunKindExternalSingle,
			RepoID: repoID,
			SHA:    sha,
		},
		Coverage: payload,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.CoverageSamplesWritten != 2 {
		t.Errorf("out.CoverageSamplesWritten = %d; want 2", out.CoverageSamplesWritten)
	}
	if out.CoverageRowsHydrated != 2 {
		t.Errorf("out.CoverageRowsHydrated = %d; want 2", out.CoverageRowsHydrated)
	}
	if out.FoundationDispatched {
		t.Errorf("out.FoundationDispatched = true; want false (external_single never dispatches foundation)")
	}
	if out.ChurnSamplesWritten != 0 || out.ChurnRowsHydrated != 0 {
		t.Errorf("out.Churn{Samples,Rows} = %d/%d; want 0/0 (external_single coverage path does not write churn)", out.ChurnSamplesWritten, out.ChurnRowsHydrated)
	}
}
